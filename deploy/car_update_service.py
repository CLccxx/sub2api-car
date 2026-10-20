#!/usr/bin/env python3
"""Root host updater for the fixed car release source (Python 3.9+).

Only a Unix socket is served. Preparation keeps the application running;
activation delegates every backup, switch, health check and recovery decision
to upgrade_car. SIGTERM drains an active transaction rather than interrupting
its worker. systemd must allow the drain to finish (TimeoutStopSec=infinity).
"""
import argparse
from dataclasses import dataclass
import errno
import fcntl
import hashlib
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import platform
import re
import signal
import socket
import socketserver
import stat
import struct
import sys
import tempfile
import threading
import uuid

import car_release
import upgrade_car

PROTOCOL_VERSION = 1
MAX_BODY_BYTES = 1024
MAX_STATE_BYTES = 64 * 1024
# Recovery has two metadata-bounded ledgers (4x including JSON formatting),
# plus old/new manifest path text in both managed/changed (12x with escaping).
# Reserve 256 bookkeeping bytes per member of that union and fixed status data.
# Daemon state/receipts keep their much smaller independent limit above.
MAX_RECOVERY_BYTES = (16 * upgrade_car.MAX_METADATA_BYTES
                      + 512 * upgrade_car.MAX_MEMBERS + MAX_STATE_BYTES)
OPERATION_RE = re.compile(r"[A-Za-z0-9._:-]{1,160}\Z")
IMAGE_RE = re.compile(r"sha256:[a-f0-9]{64}\Z")
PHASES = frozenset({"idle", "preparing", "ready", "applying", "succeeded", "failed", "rolled_back", "manual_recovery_required"})
MESSAGES = {
    "INVALID_REQUEST": "The updater request is invalid.",
    "UPDATER_BUSY": "Another update operation is in progress.",
    "UPDATER_STOPPING": "The updater is restarting. Try again shortly.",
    "OPERATION_CONFLICT": "This operation identifier was already used for a different version.",
    "STALE_OPERATION": "This operation is no longer the prepared update.",
    "NOT_READY": "The update has not finished preparation.",
    "VERSION_NOT_NEWER": "The requested release is not newer than the running application.",
    "RELEASE_UNAVAILABLE": "The latest complete release could not be downloaded and verified.",
    "PREPARATION_FAILED": "Update preparation failed. The running application was not changed.",
    "ACTIVATION_FAILED": "Update activation failed before switching the application.",
    "ROLLED_BACK": "The previous application was restored and verified healthy. The database was not restored.",
    "MANUAL_RECOVERY_REQUIRED": "The update requires operator recovery. Inspect the retained backup and recovery record.",
    "OPERATION_INTERRUPTED": "The updater was interrupted. The operation was not replayed.",
    "STATE_INVALID": "The updater state or storage permissions are invalid.",
    "PEER_NOT_ALLOWED": "This local client is not permitted to use the updater.",
}
PHASE_MESSAGES = {"idle": "No update is in progress.", "preparing": "Preparing the update while the application keeps running.",
                  "ready": "The verified update is ready to activate.", "applying": "Backing up and activating the prepared update.",
                  "succeeded": "The updated application is running and verified healthy."}


class ServiceError(Exception):
    def __init__(self, code, http_status=400):
        self.code, self.http_status = code, http_status
        super().__init__(MESSAGES[code])

    def body(self):
        return {"error_code": self.code, "message": str(self)}


@dataclass
class ServiceConfig:
    install_dir: Path = Path("/opt/sub2api-car")
    updates_dir: Path = Path("/opt/sub2api-car-updates")
    backups_dir: Path = Path("/opt/sub2api-car-backups")
    state_dir: Path = Path("/var/lib/sub2api-car-updater")


def _operation(value):
    if not isinstance(value, str) or not OPERATION_RE.fullmatch(value):
        raise ServiceError("INVALID_REQUEST")
    return value


def _status(phase="idle", operation_id="", version="", commit="", code="", backup="", step=None):
    return {"protocol_version": PROTOCOL_VERSION, "phase": phase, "operation_id": operation_id,
            "target_version": version, "target_commit": commit, "message": MESSAGES[code] if code else PHASE_MESSAGES[phase],
            "error_code": code, "backup_path": backup, "step": step or phase}


def _private_json(path, max_bytes=MAX_STATE_BYTES):
    with upgrade_car._open_regular(path) as handle:
        info = os.fstat(handle.fileno())
        if info.st_uid != os.geteuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) & 0o077:
            raise ServiceError("STATE_INVALID", 503)
        data = handle.read(max_bytes + 1)
    if len(data) > max_bytes:
        raise ServiceError("STATE_INVALID", 503)
    return json.loads(data, object_pairs_hook=upgrade_car._json_no_duplicates)


def _save_json(path, data):
    upgrade_car._atomic_file(path, data=(json.dumps(data, sort_keys=True) + "\n").encode(), mode=0o600)


class UpdaterService:
    """Serialized operation core; root and Linux are enforced by the CLI only."""
    def __init__(self, config=None, release_client=None, runner=None):
        raw = config or ServiceConfig()
        self.config = ServiceConfig(*(Path(os.path.abspath(p)) for p in
                                      (raw.install_dir, raw.updates_dir, raw.backups_dir, raw.state_dir)))
        paths = tuple(vars(self.config).values())
        for index, first in enumerate(paths):
            for second in paths[index + 1:]:
                if first == second or first in second.parents or second in first.parents:
                    raise ServiceError("STATE_INVALID", 503)
        self.release_client = release_client or car_release.ReleaseClient()
        self.runner = runner or upgrade_car.SubprocessRunner()
        self._mutex = threading.RLock()
        self._worker = None
        self._lock_fd = None
        self.stopping = threading.Event()
        self.reload_requested = threading.Event()
        self._state = {"format_version": 1, "status": _status(), "prepared": None, "job_key": "", "activated": False}
        try:
            root = upgrade_car._private_directory(self.config.state_dir)
            self._records = upgrade_car._private_directory(root / "operations")
            fd = os.open(str(root / "daemon.lock"), os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
            self._lock_fd = fd
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid()
                    or stat.S_IMODE(info.st_mode) != 0o600):
                raise ServiceError("STATE_INVALID", 503)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ServiceError("UPDATER_BUSY", 503) from None
            state_path = root / "state.json"
            if state_path.exists() or state_path.is_symlink():
                self._state = _private_json(state_path)
                self._validate_state()
                phase = self._state["status"]["phase"]
                if phase == "preparing":
                    self._transition("failed", code="OPERATION_INTERRUPTED")
                elif phase == "applying":
                    self._recover(interrupted=True)
            else:
                self._persist()
        except (OSError, ValueError, TypeError, KeyError, UnicodeError, car_release.ReleaseError, upgrade_car.UpgradeError, ServiceError):
            self._release_lock()
            raise ServiceError("STATE_INVALID", 503) from None

    def _validate_state(self):
        state = self._state
        if not isinstance(state, dict) or set(state) != {"format_version", "status", "prepared", "job_key", "activated"}:
            raise ServiceError("STATE_INVALID", 503)
        status = state["status"]
        if (type(state["format_version"]) is not int or state["format_version"] != 1 or type(state["activated"]) is not bool
                or not isinstance(status, dict) or set(status) != set(_status())
                or type(status["protocol_version"]) is not int or status["protocol_version"] != 1
                or any(not isinstance(v, str) or len(v) > 4096 for k, v in status.items() if k != "protocol_version")
                or status["phase"] not in PHASES):
            raise ServiceError("STATE_INVALID", 503)
        if status["phase"] != "idle":
            _operation(status["operation_id"])
            car_release.version_key(status["target_version"])
            if not isinstance(state["job_key"], str) or not re.fullmatch(r"[a-f0-9]{32}", state["job_key"]):
                raise ServiceError("STATE_INVALID", 503)
        if status["target_commit"] and not upgrade_car.COMMIT_RE.fullmatch(status["target_commit"]):
            raise ServiceError("STATE_INVALID", 503)
        if status["error_code"] and status["error_code"] not in MESSAGES:
            raise ServiceError("STATE_INVALID", 503)
        if status["backup_path"]:
            path = Path(status["backup_path"])
            if not path.is_absolute() or self._backup_root() not in path.parents or ".." in path.parts:
                raise ServiceError("STATE_INVALID", 503)
        prepared = state["prepared"]
        if prepared is not None:
            if (not isinstance(prepared, dict) or set(prepared) != {"archive", "sha256", "version", "commit", "image_id"}
                    or any(not isinstance(v, str) for v in prepared.values())
                    or not upgrade_car.HASH_RE.fullmatch(prepared["sha256"])
                    or (prepared["version"], prepared["commit"]) != (status["target_version"], status["target_commit"])
                    or not IMAGE_RE.fullmatch(prepared["image_id"])):
                raise ServiceError("STATE_INVALID", 503)
            path = Path(prepared["archive"])
            if not path.is_absolute() or self.config.updates_dir not in path.parents or ".." in path.parts or path.name != "bundle.tar.gz":
                raise ServiceError("STATE_INVALID", 503)
        if status["phase"] in {"ready", "applying", "succeeded", "rolled_back"} and prepared is None:
            raise ServiceError("STATE_INVALID", 503)

    def _receipt_path(self, operation):
        return self._records / (hashlib.sha256(operation.encode()).hexdigest() + ".json")

    def _state_invalid(self):
        self.stopping.set()
        previous = self._state["status"]
        self._state["status"] = _status("manual_recovery_required", previous["operation_id"],
                                         previous["target_version"], previous["target_commit"],
                                         "STATE_INVALID", previous["backup_path"])

    def _persist(self):
        try:
            _save_json(self.config.state_dir / "state.json", self._state)
            status = self._state["status"]
            if status["operation_id"]:
                _save_json(self._receipt_path(status["operation_id"]), status)
        except (OSError, upgrade_car.UpgradeError):
            # Never expose ready/succeeded based only on a volatile transition.
            # A restart reconciles the last durable state without replaying it.
            self._state_invalid()
            raise ServiceError("STATE_INVALID", 503) from None

    def _transition(self, phase, code="", commit=None, backup=None, step=None):
        with self._mutex:
            previous = self._state["status"]
            self._state["status"] = _status(phase, previous["operation_id"], previous["target_version"],
                                             previous["target_commit"] if commit is None else commit, code,
                                             previous["backup_path"] if backup is None else backup, step)
            self._persist()

    def status(self):
        with self._mutex:
            return dict(self._state["status"])

    def _available(self):
        if self.stopping.is_set() or self.reload_requested.is_set():
            raise ServiceError("UPDATER_STOPPING", 503)

    def _run_job(self, target):
        try:
            target()
        except (Exception, KeyboardInterrupt):
            # The job methods classify transaction outcomes. An exception that
            # escapes them means even the private outcome could not be saved.
            with self._mutex:
                self._state_invalid()

    def _start(self, target):
        self._worker = threading.Thread(target=self._run_job, args=(target,), name="car-update-operation", daemon=False)
        self._worker.start()

    def prepare(self, operation_id, version):
        _operation(operation_id)
        try:
            car_release.version_key(version)
        except car_release.ReleaseError:
            raise ServiceError("INVALID_REQUEST") from None
        with self._mutex:
            previous = self._state["status"]
            if previous["operation_id"] == operation_id:
                if previous["target_version"] != version:
                    raise ServiceError("OPERATION_CONFLICT", 409)
                return dict(previous)
            receipt_path = self._receipt_path(operation_id)
            if receipt_path.exists():
                receipt = _private_json(receipt_path)
                if receipt["target_version"] != version:
                    raise ServiceError("OPERATION_CONFLICT", 409)
                return dict(receipt)
            self._available()
            if previous["phase"] in {"preparing", "applying"}:
                raise ServiceError("UPDATER_BUSY", 409)
            if previous["phase"] == "manual_recovery_required":
                raise ServiceError("MANUAL_RECOVERY_REQUIRED", 409)
            if previous["phase"] == "ready":
                _save_json(self._receipt_path(previous["operation_id"]),
                           _status("failed", previous["operation_id"], previous["target_version"], previous["target_commit"], "STALE_OPERATION"))
            self._state = {"format_version": 1, "status": _status("preparing", operation_id, version),
                           "prepared": None, "job_key": uuid.uuid4().hex, "activated": False}
            self._persist()
            result = dict(self._state["status"])
            self._start(self._prepare)
            return result

    def activate(self, operation_id):
        _operation(operation_id)
        with self._mutex:
            previous = self._state["status"]
            if previous["operation_id"] != operation_id:
                raise ServiceError("STALE_OPERATION", 409)
            if self._state["activated"]:
                return dict(previous)
            self._available()
            if previous["phase"] != "ready":
                raise ServiceError("NOT_READY", 409)
            self._state["activated"] = True
            self._transition("applying")
            result = dict(self._state["status"])
            self._start(self._activate)
            return result

    def _upgrade_config(self, dedicated_backups=False):
        return upgrade_car.UpgradeConfig(self.config.install_dir, self.config.updates_dir,
                                         self._backup_root() if dedicated_backups else self.config.backups_dir)

    def _backup_root(self):
        return self.config.backups_dir / ("web-" + self._state["job_key"])

    def _prepare(self):
        try:
            config = self._upgrade_config()
            # This may create only the helper's private control lock; it never
            # changes app/config/data. It also serializes manual CLI upgrades.
            with upgrade_car.installation_lock(config.install_dir):
                current = upgrade_car._running_app(config, self.runner)
                version = self._state["status"]["target_version"]
                if car_release.version_key(version) <= car_release.version_key(current["version"]):
                    raise ServiceError("VERSION_NOT_NEWER", 409)
                if not current["healthy"]:
                    raise ServiceError("PREPARATION_FAILED", 409)
                self._transition("preparing", step="downloading")
                downloads = upgrade_car._private_directory(self.config.state_dir / "downloads")
                download_dir = Path(tempfile.mkdtemp(prefix="release-", dir=str(downloads)))
                downloaded = self.release_client.download_latest(version, download_dir)
                self._transition("preparing", step="verifying")
                stage, release, bundle = upgrade_car._stage_archive(downloaded.archive, downloaded.sha256, config.updates_dir)
                if bundle.version != version:
                    raise ServiceError("PREPARATION_FAILED", 409)
                self._transition("preparing", commit=bundle.commit, step="building")
                image_tag = "sub2api-car:" + bundle.version
                upgrade_car._run(self.runner, ["docker", "build", "-f", "Dockerfile.goreleaser", "-t", image_tag, "."],
                                 "Image build", cwd=release, timeout=1800)
                inspected = upgrade_car._run(self.runner, ["docker", "image", "inspect", "--format", "{{.Id}}", image_tag], "Inspect prepared image")
                image_id = inspected.stdout.decode("ascii").strip()
                if not IMAGE_RE.fullmatch(image_id):
                    raise ServiceError("PREPARATION_FAILED", 409)
                actual = upgrade_car._version_output(upgrade_car._run(self.runner,
                        ["docker", "run", "--rm", "--network", "none", "--read-only", "--entrypoint", "/app/sub2api", image_id, "--version"],
                        "Verify prepared image", timeout=60))
                if actual != (bundle.version, bundle.commit):
                    raise ServiceError("PREPARATION_FAILED", 409)
                with self._mutex:
                    self._state["prepared"] = {"archive": str(bundle.archive), "sha256": bundle.sha256,
                                               "version": bundle.version, "commit": bundle.commit, "image_id": image_id}
            # Publish readiness only after releasing the installation lock so
            # immediate activation cannot race the end of preparation.
            self._transition("ready")
        except car_release.ReleaseError:
            self._transition("failed", code="RELEASE_UNAVAILABLE")
        except ServiceError as error:
            if error.code != "STATE_INVALID":
                self._transition("failed", code=error.code)
        except (Exception, KeyboardInterrupt):
            self._transition("failed", code="PREPARATION_FAILED")

    def _activate(self):
        try:
            pin = dict(self._state["prepared"])
            config = self._upgrade_config(dedicated_backups=True)
            # A separately invoked upgrade may have moved the installation since
            # preparation. Never turn a stale prepared release into a downgrade.
            running = upgrade_car._running_app(config, self.runner)
            if car_release.version_key(running["version"]) > car_release.version_key(pin["version"]):
                raise ServiceError("VERSION_NOT_NEWER", 409)
            image = upgrade_car.PreparedImage(pin["sha256"], pin["version"], pin["commit"], pin["image_id"])
            result = upgrade_car.upgrade(Path(pin["archive"]), config, sha256=pin["sha256"], runner=self.runner,
                                         report=lambda message: None, prepared_image=image)
            with self._mutex:
                self._transition("succeeded", backup=str(result.backup_dir) if result.backup_dir else "")
                # The CLI drains the worker and HTTP requests before exiting.
                # Restart=always reloads all three installed Python modules.
                self.reload_requested.set()
        except (Exception, KeyboardInterrupt):
            self._recover(interrupted=False)

    def _recovery_record(self):
        root = self._backup_root()
        if not root.exists():
            return None, None
        upgrade_car._safe_directory(root)
        paths = list(root.glob("*/recovery.json"))
        if len(paths) != 1:
            raise ServiceError("MANUAL_RECOVERY_REQUIRED", 503)
        path = paths[0]
        upgrade_car._safe_directory(path.parent)
        record = _private_json(path, max_bytes=MAX_RECOVERY_BYTES)
        status = self._state["status"]
        if (not isinstance(record, dict) or record.get("target_version") != status["target_version"]
                or record.get("target_commit") != status["target_commit"]):
            raise ServiceError("MANUAL_RECOVERY_REQUIRED", 503)
        return path.parent, record

    def _recover(self, interrupted):
        phase = "manual_recovery_required" if interrupted else "failed"
        code = "OPERATION_INTERRUPTED" if interrupted else "ACTIVATION_FAILED"
        backup = ""
        try:
            path, record = self._recovery_record()
            if record is not None:
                backup = str(path)
                outcome = record.get("status")
                if outcome in {"complete", "rolled_back"}:
                    current = upgrade_car._running_app(self._upgrade_config(), self.runner)
                    expected = (self._state["prepared"] if outcome == "complete" else record.get("previous_app", {}))
                    verified = current["healthy"] and all(current[key] == expected.get(key) for key in ("version", "commit", "image_id"))
                    if verified:
                        phase, code = ("succeeded", "") if outcome == "complete" else ("rolled_back", "ROLLED_BACK")
                    else:
                        phase, code = "manual_recovery_required", "MANUAL_RECOVERY_REQUIRED"
                elif outcome not in {"preparing", "prepared"} or record.get("changed"):
                    phase, code = "manual_recovery_required", "MANUAL_RECOVERY_REQUIRED"
            with self._mutex:
                self._transition(phase, code=code, backup=backup)
                if phase == "succeeded":
                    # A transient first success-save failure may have set
                    # stopping. Verified recovery must still reload all modules
                    # instead of leaving a successful but unavailable daemon.
                    self.reload_requested.set()
        except (Exception, KeyboardInterrupt):
            self._transition("manual_recovery_required", code="MANUAL_RECOVERY_REQUIRED", backup=backup)

    def wait_for_job(self, timeout=None):
        with self._mutex:
            worker = self._worker
        if worker is not None:
            worker.join(timeout)
        return worker is None or not worker.is_alive()

    def _release_lock(self):
        if self._lock_fd is not None:
            os.close(self._lock_fd)
            self._lock_fd = None

    def close(self):
        self.stopping.set()
        self.wait_for_job()
        with self._mutex:
            self._release_lock()


def allowed_peer(connection, app_uid=1000):
    if sys.platform != "linux" or not hasattr(socket, "SO_PEERCRED"):
        return False
    try:
        unused_pid, uid, unused_gid = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i")))
        return uid in {0, app_uid}
    except (OSError, ValueError, struct.error):
        return False


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"
    server_version = "car-updater"
    sys_version = ""

    def setup(self):
        self.request.settimeout(5)
        super().setup()

    def log_message(self, format, *args):
        pass  # Paths, bodies and child logs are never written to the journal.

    def _reply(self, code, data):
        self.close_connection = True
        payload = (json.dumps(data, separators=(",", ":")) + "\n").encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)

    def send_error(self, code, message=None, explain=None):
        self._reply(400, ServiceError("INVALID_REQUEST").body())

    def _body(self, expected_fields):
        lengths = self.headers.get_all("Content-Length", [])
        if (self.headers.get_all("Transfer-Encoding") or len(lengths) != 1 or not re.fullmatch(r"[0-9]{1,5}", lengths[0])
                or int(lengths[0]) > MAX_BODY_BYTES or self.headers.get_content_type() != "application/json"):
            raise ServiceError("INVALID_REQUEST")
        data = self.rfile.read(int(lengths[0]))
        if len(data) != int(lengths[0]):
            raise ServiceError("INVALID_REQUEST")
        try:
            body = json.loads(data, object_pairs_hook=upgrade_car._json_no_duplicates)
            if not isinstance(body, dict) or set(body) != expected_fields:
                raise ValueError()
            return body
        except (ValueError, UnicodeError, upgrade_car.UpgradeError):
            raise ServiceError("INVALID_REQUEST") from None

    def do_GET(self):
        try:
            if (self.path != "/v1/status" or self.headers.get_all("Transfer-Encoding")
                    or self.headers.get_all("Content-Length", []) not in ([], ["0"])):
                raise ServiceError("INVALID_REQUEST")
            self._reply(200, self.server.service.status())
        except ServiceError as error:
            self._reply(error.http_status, error.body())

    def do_POST(self):
        try:
            if self.path == "/v1/prepare":
                body = self._body({"operation_id", "version"})
                status = self.server.service.prepare(body["operation_id"], body["version"])
            elif self.path == "/v1/activate":
                body = self._body({"operation_id"})
                status = self.server.service.activate(body["operation_id"])
            else:
                raise ServiceError("INVALID_REQUEST")
            self._reply(202, status)
        except ServiceError as error:
            self._reply(error.http_status, error.body())
        except (OSError, ValueError, KeyError, TypeError, upgrade_car.UpgradeError):
            self._reply(503, ServiceError("STATE_INVALID", 503).body())


class UnixUpdateServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = False
    block_on_close = True
    request_queue_size = 16

    def __init__(self, socket_path, service, app_uid=1000, app_gid=None, peer_checker=None):
        self.service = service
        self.peer_checker = peer_checker or (lambda connection: allowed_peer(connection, app_uid))
        self._slots = threading.BoundedSemaphore(16)
        self.socket_path = Path(socket_path)
        if not self.socket_path.is_absolute():
            raise ServiceError("STATE_INVALID", 503)
        parent = upgrade_car._safe_directory(self.socket_path.parent, create=True, mode=0o750)
        parent_info = parent.stat()
        if parent_info.st_uid != os.geteuid() or stat.S_IMODE(parent_info.st_mode) & 0o022:
            raise ServiceError("STATE_INVALID", 503)
        if self.socket_path.exists() or self.socket_path.is_symlink():
            info = self.socket_path.lstat()
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid():
                raise ServiceError("STATE_INVALID", 503)
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as probe:
                probe.settimeout(0.2)
                try:
                    probe.connect(str(self.socket_path))
                except OSError as error:
                    if error.errno != errno.ECONNREFUSED:
                        raise ServiceError("UPDATER_BUSY", 503) from None
                else:
                    raise ServiceError("UPDATER_BUSY", 503)
            self.socket_path.unlink()
        super().__init__(str(self.socket_path), _Handler)
        try:
            if os.geteuid() == 0:
                group = app_uid if app_gid is None else app_gid
                os.chown(parent, 0, group)
                os.chmod(parent, 0o750)
                os.chown(self.socket_path, 0, group)
            os.chmod(self.socket_path, 0o660)
        except BaseException:
            self.server_close()
            raise

    def verify_request(self, request, client_address):
        request.settimeout(5)
        if self.peer_checker(request):
            return True
        payload = (json.dumps(ServiceError("PEER_NOT_ALLOWED", 503).body()) + "\n").encode()
        try:
            request.sendall(b"HTTP/1.0 503 Service Unavailable\r\nContent-Type: application/json\r\nContent-Length: "
                            + str(len(payload)).encode() + b"\r\nConnection: close\r\n\r\n" + payload)
        except OSError:
            pass
        return False

    def process_request(self, request, client_address):
        if not self._slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self._slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._slots.release()

    def handle_error(self, request, client_address):
        pass

    def server_close(self):
        super().server_close()
        try:
            if stat.S_ISSOCK(self.socket_path.lstat().st_mode):
                self.socket_path.unlink()
        except FileNotFoundError:
            pass


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--install-dir", type=Path, default=ServiceConfig.install_dir)
    parser.add_argument("--updates-dir", type=Path, default=ServiceConfig.updates_dir)
    parser.add_argument("--backups-dir", type=Path, default=ServiceConfig.backups_dir)
    parser.add_argument("--state-dir", type=Path, default=ServiceConfig.state_dir)
    parser.add_argument("--socket", type=Path, default=Path("/run/sub2api-car-updater/control.sock"))
    parser.add_argument("--app-uid", type=int, default=1000)
    parser.add_argument("--app-gid", type=int, default=1000)
    args = parser.parse_args(argv)
    if os.geteuid() != 0 or sys.platform != "linux" or platform.machine() not in {"x86_64", "amd64"}:
        print("The host updater requires root on Linux x86_64.", file=sys.stderr)
        return 1
    if args.app_uid < 0 or args.app_gid < 0:
        print("The configured application identity is invalid.", file=sys.stderr)
        return 1
    updater, server = None, None
    shutdown = threading.Event()
    previous = {}
    def stop(signum, frame):
        shutdown.set()
        if updater is not None:
            updater.stopping.set()
    try:
        for signum in (signal.SIGTERM, signal.SIGINT):
            previous[signum] = signal.signal(signum, stop)
        updater = UpdaterService(ServiceConfig(args.install_dir, args.updates_dir, args.backups_dir, args.state_dir))
        server = UnixUpdateServer(args.socket, updater, app_uid=args.app_uid, app_gid=args.app_gid)
        server.timeout = 0.5
        while not shutdown.is_set() and not updater.reload_requested.is_set():
            server.handle_request()
        return 0
    except (ServiceError, OSError, upgrade_car.UpgradeError):
        print("The host updater could not start or persist its private state.", file=sys.stderr)
        return 1
    finally:
        if updater is not None:
            updater.close()
        if server is not None:
            server.server_close()
        for signum, handler in previous.items():
            signal.signal(signum, handler)


if __name__ == "__main__":
    sys.exit(main())
