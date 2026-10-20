#!/usr/bin/env python3
"""Install web updates from a trusted local car bundle (Python 3.9+, Linux amd64).

This script is standalone: no adjacent Python modules are required. The archive
and SHA256 sidecar must come from a trusted source; a checksum proves integrity,
not authorship. No URL is accepted and child-process output is never printed.
"""
import argparse
import contextlib
from dataclasses import dataclass
import fcntl
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shlex
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import time

MAX_ARCHIVE_BYTES = 1024 * 1024 * 1024
MAX_EXPANDED_BYTES = 2 * 1024 * 1024 * 1024
MAX_FILE_BYTES = 512 * 1024 * 1024
MAX_MEMBERS = 10000
MAX_HELPER_BYTES = 2 * 1024 * 1024
MAX_TAR_METADATA_BYTES = 16 * 1024
MAX_TOTAL_TAR_METADATA_BYTES = 16 * 1024 * 1024
CHUNK_BYTES = 1024 * 1024
MAX_CONFIG_BYTES = 64 * 1024
SERVICE = "sub2api-car-updater.service"
OVERLAY = "deploy/docker-compose.car-updater.yml"
CONTAINER_DIRECTORY = "/run/sub2api-car-updater"
WEB_FILES = frozenset({"deploy/upgrade_car.py", "deploy/car_release.py", "deploy/car_update_service.py",
                       "deploy/install_car_web_update.py", "deploy/" + SERVICE, OVERLAY, "MIGRATIONS.json"})


class InstallError(Exception):
    """A bounded, credential-free operator error."""


@dataclass
class InstallConfig:
    install_dir: Path = Path("/opt/sub2api-car")
    updates_dir: Path = Path("/opt/sub2api-car-updates")
    backups_dir: Path = Path("/opt/sub2api-car-backups")
    state_dir: Path = Path("/var/lib/sub2api-car-updater")
    socket: Path = Path("/run/sub2api-car-updater/control.sock")
    app_uid: int = 1000
    app_gid: int = 1000


def _regular(path):
    fd = os.open(str(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise InstallError("Expected a regular local file, without symlinks or special files.")
        return os.fdopen(fd, "rb")
    except BaseException:
        os.close(fd)
        raise


class _BootstrapTarInfo(tarfile.TarInfo):
    def _proc_member(self, archive):
        archive._bootstrap_headers = getattr(archive, "_bootstrap_headers", 0) + 1
        if archive._bootstrap_headers > MAX_MEMBERS:
            raise InstallError("Archive has too many headers.")
        if self.type in (tarfile.XHDTYPE, tarfile.XGLTYPE, tarfile.GNUTYPE_LONGNAME, tarfile.GNUTYPE_LONGLINK):
            total = getattr(archive, "_bootstrap_metadata", 0) + self.size
            archive._bootstrap_metadata = total
            if self.size < 0 or self.size > MAX_TAR_METADATA_BYTES or total > MAX_TOTAL_TAR_METADATA_BYTES:
                raise InstallError("Archive extension metadata is too large.")
        elif self.type not in (tarfile.REGTYPE, tarfile.AREGTYPE, tarfile.DIRTYPE):
            raise InstallError("Archive contains a link or special file.")
        elif self.size < 0 or self.size > MAX_FILE_BYTES or (self.isdir() and self.size):
            raise InstallError("Archive member size is invalid.")
        return super()._proc_member(archive)


def _copy_verified_archive(archive, destination, sha256):
    if sha256 is None:
        with _regular(Path(str(archive) + ".sha256")) as source:
            sidecar = source.read(4097)
        match = re.fullmatch(rb"([a-f0-9]{64})  ([^\r\n]+)\n?", sidecar)
        if len(sidecar) > 4096 or not match or match[2].decode("utf-8") != archive.name:
            raise InstallError("A valid ARCHIVE.sha256 sidecar naming this archive is required.")
        sha256 = match[1].decode("ascii")
    if not isinstance(sha256, str) or not re.fullmatch(r"[a-f0-9]{64}", sha256):
        raise InstallError("--sha256 must be 64 lowercase hexadecimal characters.")
    digest, length = hashlib.sha256(), 0
    with _regular(archive) as source, destination.open("xb") as output:
        os.fchmod(output.fileno(), 0o600)
        while True:
            data = source.read(CHUNK_BYTES)
            if not data:
                break
            length += len(data)
            if length > MAX_ARCHIVE_BYTES:
                raise InstallError("Archive is too large.")
            digest.update(data)
            output.write(data)
        output.flush()
        os.fsync(output.fileno())
    if digest.hexdigest() != sha256:
        raise InstallError("Archive SHA256 does not match the expected checksum.")
    return sha256


def _bootstrap_helper(archive, directory):
    """Bound and scan every entry before executing the one fixed helper path."""
    helper, seen, files, total = None, set(), set(), 0
    with tarfile.open(archive, "r|gz", tarinfo=_BootstrapTarInfo) as tar:
        for number, item in enumerate(tar, 1):
            name = item.name.removesuffix("/") if item.isdir() else item.name
            if (number > MAX_MEMBERS or not name or len(name) > 512 or name.startswith("/")
                    or "\\" in name or any(ord(c) < 32 or ord(c) == 127 for c in name)
                    or str(PurePosixPath(name)) != name or any(p in {"", ".", ".."} for p in name.split("/"))
                    or not (name == "sub2api-car" or name.startswith("sub2api-car/"))):
                raise InstallError("Archive contains an unsafe or unexpected path.")
            if name in seen:
                raise InstallError("Archive contains duplicate paths.")
            seen.add(name)
            if item.isdir():
                continue
            if not item.isreg() or item.issparse() or item.linkname:
                raise InstallError("Archive contains a link or special file.")
            files.add(name)
            total += item.size
            if total > MAX_EXPANDED_BYTES:
                raise InstallError("Archive expanded size is too large.")
            if name == "sub2api-car/deploy/upgrade_car.py":
                if item.size > MAX_HELPER_BYTES:
                    raise InstallError("Bootstrap helper is too large.")
                helper = tar.extractfile(item).read(MAX_HELPER_BYTES + 1)
    if any(str(parent) in files for name in seen for parent in PurePosixPath(name).parents):
        raise InstallError("Archive has conflicting file and directory paths.")
    if helper is None:
        raise InstallError("The bundle is missing the bootstrap upgrade helper.")
    target = directory / "upgrade_car.py"
    with target.open("xb") as output:
        os.fchmod(output.fileno(), 0o600)
        output.write(helper)
    spec = importlib.util.spec_from_file_location("_car_verified_upgrade", target)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@contextlib.contextmanager
def verified_bundle(archive, sha256=None):
    """Load the verified release helper from a private copy, never adjacent files."""
    helper = None
    try:
        with tempfile.TemporaryDirectory(prefix="car-web-bootstrap-") as temporary:
            directory = Path(temporary).resolve()
            directory.chmod(0o700)
            private_archive = directory / "bundle.tar.gz"
            checksum = _copy_verified_archive(Path(archive), private_archive, sha256)
            helper = _bootstrap_helper(private_archive, directory)
            bundle = helper.verify_archive(private_archive, checksum)
            if not WEB_FILES.issubset(bundle.files):
                raise InstallError("A complete web-updater bundle, including migration metadata, is required.")
            unused_stage, release, bundle = helper._stage_archive(private_archive, checksum, directory / "staging")
            yield helper, release, bundle
    except InstallError:
        raise
    except (OSError, ValueError, UnicodeError, tarfile.TarError, EOFError, ImportError, AttributeError):
        raise InstallError("Unable to load a complete, valid local web-updater archive.") from None
    except Exception as error:
        if helper is not None and isinstance(error, helper.UpgradeError):
            raise InstallError(str(error)) from None
        raise


def _path(value):
    text = str(value)
    if (not text.startswith("/") or text == "/" or not re.fullmatch(r"/[A-Za-z0-9_./-]+", text)
            or any(part in {"", ".", ".."} for part in text[1:].split("/"))):
        raise InstallError("Use absolute canonical paths containing only letters, digits, slash, dot, underscore or hyphen.")
    return Path(text)


def _owned(info):
    if info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) & 0o022:
        raise InstallError("Updater paths must be root-owned and not writable by other users.")


def _directory_preflight(helper, path):
    existing = path
    while not existing.exists() and not existing.is_symlink():
        existing = existing.parent
    helper._safe_directory(existing)
    _owned(existing.stat())


def _settings_file(helper, path):
    helper._safe_directory(path.parent)
    if not path.exists() and not path.is_symlink():
        return None
    with helper._open_regular(path) as source:
        info = os.fstat(source.fileno())
        _owned(info)
        if info.st_nlink != 1:
            raise InstallError("Updater settings must not be hard-linked.")
        data = source.read(MAX_CONFIG_BYTES + 1)
    if len(data) > MAX_CONFIG_BYTES:
        raise InstallError("Updater settings file is too large.")
    return {"data": data, "mode": stat.S_IMODE(info.st_mode), "uid": info.st_uid, "gid": info.st_gid}


def _service_command(config):
    command = ["/usr/bin/python3", str(config.install_dir / "deploy/car_update_service.py")]
    for flag, value in (("--install-dir", config.install_dir), ("--updates-dir", config.updates_dir),
                        ("--backups-dir", config.backups_dir), ("--state-dir", config.state_dir),
                        ("--socket", config.socket), ("--app-uid", config.app_uid), ("--app-gid", config.app_gid)):
        command.extend([flag, str(value)])
    return command


def _validate_unit(data, config):
    section, service = "", {}
    for line in data.decode("utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith(("#", ";")):
            continue
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1]
        elif section == "Service":
            if "=" not in line:
                raise InstallError("The existing systemd unit is not a compatible updater unit.")
            key, value = line.split("=", 1)
            if key in service:
                raise InstallError("Duplicate systemd service settings require operator review.")
            service[key] = value
    required = {"User": "root", "Group": "root", "Restart": "always", "KillMode": "mixed",
                "TimeoutStopSec": "infinity", "UMask": "0077"}
    if (any(service.get(k) != v for k, v in required.items())
            or shlex.split(service.get("ExecStart", "")) != _service_command(config)
            or any(key.startswith("Exec") and key != "ExecStart" for key in service)
            or ("RuntimeDirectory" in service and service.get("RuntimeDirectoryPreserve") != "yes")):
        raise InstallError("The existing systemd unit is unrelated or incompatible; preserve it and reconcile its settings before retrying.")


def _render_settings(release, config):
    unit = (release / ("deploy/" + SERVICE)).read_text()
    lines = ["ExecStart=" + " ".join(_service_command(config)) if line.startswith("ExecStart=") else line
             for line in unit.splitlines()]
    unit_data = ("\n".join(lines) + "\n").encode()
    _validate_unit(unit_data, config)
    overlay = (release / OVERLAY).read_text()
    overlay = overlay.replace("source: /run/sub2api-car-updater", "source: " + str(config.socket.parent))
    overlay = overlay.replace("UPDATE_MANAGED_SOCKET: /run/sub2api-car-updater/control.sock",
                              "UPDATE_MANAGED_SOCKET: " + CONTAINER_DIRECTORY + "/" + config.socket.name)
    return unit_data, overlay.encode()


def _mount_matches(mount, config, runtime=False):
    keys = ("Type", "Source", "Destination", "RW") if runtime else ("type", "source", "target", "read_only")
    return (isinstance(mount, dict) and mount.get(keys[0]) == "bind" and mount.get(keys[1]) == str(config.socket.parent)
            and mount.get(keys[2]) == CONTAINER_DIRECTORY and mount.get(keys[3]) is (False if runtime else True))


def _validate_compose(helper, core, config, runner, candidate, has_overlay):
    command = helper._compose(core)
    if not has_overlay:
        command.extend(["-f", str(candidate)])
    result = helper._run(runner, command + ["config", "--format", "json"], "Validate updater Compose configuration")
    if len(result.stdout) > helper.MAX_METADATA_BYTES:
        raise InstallError("Compose configuration is too large to validate.")
    try:
        app = json.loads(result.stdout, object_pairs_hook=helper._json_no_duplicates)["services"]["sub2api"]
        volumes = app.get("volumes", [])
        matches = [v for v in volumes if v.get("target") == CONTAINER_DIRECTORY]
        if (app["environment"].get("UPDATE_MANAGED_SOCKET") != CONTAINER_DIRECTORY + "/" + config.socket.name
                or len(matches) != 1 or not _mount_matches(matches[0], config)
                or app.get("privileged") or app.get("pid") == "host"
                or any("docker.sock" in str(v.get("source", "")) for v in volumes)):
            raise ValueError()
    except (ValueError, KeyError, TypeError, AttributeError):
        raise InstallError("Compose must give only the expected read-only updater directory mount and socket setting to the application.") from None


def _runtime_wired(helper, config, runner, old):
    result = helper._run(runner, ["docker", "inspect", "--format", "{{json .}}", old["container"]], "Verify app updater mount")
    if len(result.stdout) > helper.MAX_METADATA_BYTES:
        raise InstallError("Container configuration is too large to validate.")
    try:
        data = json.loads(result.stdout, object_pairs_hook=helper._json_no_duplicates)
        env = [v for v in data["Config"]["Env"] if v.startswith("UPDATE_MANAGED_SOCKET=")]
        mounts = [v for v in data["Mounts"] if v.get("Destination") == CONTAINER_DIRECTORY]
        configured = (env == ["UPDATE_MANAGED_SOCKET=" + CONTAINER_DIRECTORY + "/" + config.socket.name]
                      and len(mounts) == 1 and _mount_matches(mounts[0], config, runtime=True))
    except (ValueError, KeyError, TypeError, AttributeError):
        raise InstallError("Unable to inspect the application's updater environment and mount.") from None
    if not configured:
        return False
    try:
        host = config.socket.parent.stat()
    except FileNotFoundError:
        return False
    # Docker inspect retains the source *pathname* after that host directory is
    # replaced. On the supported Linux host, stat through the bind mount must
    # identify the same underlying device and inode as the current host path.
    try:
        result = helper._run(runner, ["docker", "exec", old["container"], "stat", "-c", "%d:%i", CONTAINER_DIRECTORY],
                             "Verify mounted updater directory identity", timeout=30)
    except helper.UpgradeError:
        return False
    identity = re.fullmatch(rb"([0-9]{1,20}):([0-9]{1,20})\n?", result.stdout or b"")
    return identity is not None and (int(identity[1]), int(identity[2])) == (host.st_dev, host.st_ino)


def _systemctl(helper, runner, action, query=False):
    args = ["systemctl", action] + (["--quiet"] if query else [])
    if action != "daemon-reload":
        args.append(SERVICE)
    if query:
        try:
            result = runner(args, timeout=30)
        except (OSError, subprocess.SubprocessError):
            raise InstallError("Unable to query the host updater service; command output was withheld.") from None
        if result.returncode not in (0, 1, 3, 4):
            raise InstallError("Unable to query the host updater service.")
        return result.returncode == 0
    helper._run(runner, args, "Host updater service " + action, timeout=None if action == "stop" else 60)


def _validate_loaded_unit(helper, runner, unit_path, required=False):
    """A drop-in or stale loaded unit must not defeat graceful worker shutdown."""
    names = ("FragmentPath", "DropInPaths", "KillMode", "TimeoutStopUSec", "Restart")
    command = ["systemctl", "show"] + ["--property=" + name for name in names] + [SERVICE]
    result = helper._run(runner, command, "Inspect effective updater service", timeout=30)
    if len(result.stdout) > MAX_CONFIG_BYTES:
        raise InstallError("Effective systemd settings are too large.")
    settings = {}
    for line in result.stdout.decode("utf-8").splitlines():
        key, separator, value = line.partition("=")
        if not separator or key not in names or key in settings:
            raise InstallError("Unable to validate the effective updater service.")
        settings[key] = value
    if settings.get("DropInPaths"):
        raise InstallError("The updater has systemd drop-ins; reconcile their effective settings before bootstrap.")
    if not required and not settings.get("FragmentPath"):
        return
    if (unit_path is None or settings.get("FragmentPath") != str(unit_path) or settings.get("KillMode") != "mixed"
            or settings.get("TimeoutStopUSec") != "infinity" or settings.get("Restart") != "always"):
        raise InstallError("The loaded updater unit does not have the expected path and graceful shutdown settings; reconcile it before retrying.")


def _reject_unit_dropins(system_root):
    # Without a main unit, systemd may omit existing drop-ins from `show`.
    # Check only relevant unit/type/prefix directories in standard system paths;
    # never read their contents or traverse arbitrary names supplied by a unit.
    roots = ("etc/systemd/system", "run/systemd/system", "usr/local/lib/systemd/system",
             "usr/lib/systemd/system", "lib/systemd/system", "etc/systemd/system.control",
             "run/systemd/system.control")
    names = (SERVICE + ".d", "sub2api-car-.service.d", "sub2api-.service.d", "service.d")
    for root in roots:
        for name in names:
            directory = system_root / root / name
            if directory.is_symlink():
                raise InstallError("An updater systemd drop-in directory is linked; reconcile it before bootstrap.")
            try:
                with os.scandir(directory) as entries:
                    for count, entry in enumerate(entries, 1):
                        if count > 128 or entry.name.endswith(".conf"):
                            raise InstallError("Existing systemd drop-ins may change the updater; reconcile them before bootstrap.")
            except FileNotFoundError:
                continue
            except OSError:
                raise InstallError("Unable to inspect updater systemd drop-in directories before bootstrap.") from None


@contextlib.contextmanager
def _daemon_lock(helper, state):
    helper._private_directory(state)
    fd = os.open(str(state / "daemon.lock"), os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid()
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise InstallError("The updater daemon lock is not a private owned file.")
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise InstallError("An updater daemon is still running; its operation must finish before bootstrap.") from None
        yield
    finally:
        os.close(fd)


def _record_settings(helper, config, unit, overlay, socket_info, active, enabled):
    record = Path(tempfile.mkdtemp(prefix="bootstrap-", dir=str(config.state_dir)))
    state = {"phase": "prepared", "service_was_active": active, "service_was_enabled": enabled,
             "socket_directory": socket_info, "unit": None, "overlay": None}
    for name, snapshot in (("unit", unit), ("overlay", overlay)):
        if snapshot is not None:
            helper._atomic_file(record / (name + ".before"), data=snapshot["data"])
            state[name] = {k: v for k, v in snapshot.items() if k != "data"}
    helper._atomic_file(record / "settings.json", data=(json.dumps(state, sort_keys=True) + "\n").encode())
    return record, state


def _restore_setting(helper, path, snapshot):
    if snapshot is None:
        if path.exists():
            path.unlink()
            helper._fsync_directory(path.parent)
    else:
        helper._atomic_file(path, data=snapshot["data"], mode=snapshot["mode"], owner=(snapshot["uid"], snapshot["gid"]))


def _status_probe(path):
    """Bounded direct Unix HTTP probe with a short startup retry window."""
    deadline = time.monotonic() + 15
    while True:
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                connection.settimeout(2)
                connection.connect(str(path))
                connection.sendall(b"GET /v1/status HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                response = http.client.HTTPResponse(connection)
                response.begin()
                data = response.read(MAX_CONFIG_BYTES + 1)
                if response.status != 200 or len(data) > MAX_CONFIG_BYTES:
                    raise InstallError("The host updater did not return a bounded successful status.")
                return json.loads(data)
        except (OSError, http.client.HTTPException):
            if time.monotonic() >= deadline:
                raise InstallError("The host updater socket did not become available.") from None
            time.sleep(0.2)


def _preflight(helper, release, bundle, config, runner, system_root):
    for name in ("install_dir", "updates_dir", "backups_dir", "state_dir", "socket"):
        setattr(config, name, _path(getattr(config, name)))
    if any(type(v) is not int or v < 0 or v > 2147483647 for v in (config.app_uid, config.app_gid)):
        raise InstallError("Application UID and GID must be nonnegative numeric identifiers.")
    if len(str(config.socket).encode()) > 103:
        raise InstallError("The Unix socket path is too long.")
    paths = [config.install_dir, config.updates_dir, config.backups_dir, config.state_dir, config.socket.parent]
    for index, first in enumerate(paths):
        if len(first.parts) < 3 or first in {Path("/var/lib"), Path("/var/run"), Path("/var/tmp"),
                                            Path("/private/tmp"), Path("/etc/systemd"), Path("/etc/systemd/system")}:
            raise InstallError("Updater storage and socket paths must use dedicated directories, not shared system directories.")
        for second in paths[index + 1:]:
            if first == second or first in second.parents or second in first.parents:
                raise InstallError("Installation, update, backup, state and socket directories must not overlap.")
        _directory_preflight(helper, first)
    helper._safe_directory(config.install_dir / "deploy")
    _owned((config.install_dir / "deploy").stat())
    core = helper.UpgradeConfig(config.install_dir, config.updates_dir, config.backups_dir)
    for name in helper._collect_managed(config.install_dir, bundle):
        target = helper._target(config.install_dir, name)
        if target.exists():
            _owned(target.stat())
    unit_path = system_root / "etc/systemd/system" / SERVICE
    _directory_preflight(helper, unit_path.parent)
    _reject_unit_dropins(system_root)
    unit, overlay = _settings_file(helper, unit_path), _settings_file(helper, config.install_dir / OVERLAY)
    unit_data, overlay_data = _render_settings(release, config)
    if unit is not None:
        _validate_unit(unit["data"], config)
    socket_info = None
    if config.socket.parent.exists():
        info = config.socket.parent.stat()
        socket_info = {"uid": info.st_uid, "gid": info.st_gid, "mode": stat.S_IMODE(info.st_mode), "inode": info.st_ino}
    if config.socket.exists() or config.socket.is_symlink():
        info = config.socket.lstat()
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid():
            raise InstallError("The updater socket path is occupied by an unsafe file.")
    env_path = config.install_dir / "deploy/.env"
    with helper._open_regular(env_path) as source:
        info = os.fstat(source.fileno())
        if info.st_uid != os.geteuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) & 0o077:
            raise InstallError("The installation .env must be a private root-owned regular file.")
        env = source.read(helper.MAX_METADATA_BYTES + 1)
    if len(env) > helper.MAX_METADATA_BYTES:
        raise InstallError("The installation .env is too large.")
    old = helper._running_app(core, runner)
    def version_key(version):
        head, tail = version.split("-car.")
        return tuple(int(v) for v in head.split(".")) + (int(tail),)
    if (old["version"] != helper._env_version(env)[0] or not old["healthy"]
            or version_key(old["version"]) > version_key(bundle.version)
            or (old["version"] == bundle.version and old["commit"] != bundle.commit)):
        raise InstallError("Bootstrap requires a healthy matching installation and cannot downgrade or reuse a version for another commit.")
    wired = _runtime_wired(helper, config, runner, old)
    for name in WEB_FILES - {OVERLAY, "MIGRATIONS.json"}:
        target = helper._target(config.install_dir, name)
        if not target.exists():
            wired = False
        else:
            with helper._open_regular(target) as installed, helper._open_regular(release / name) as incoming:
                if helper._hash_stream(installed, helper.MAX_FILE_BYTES) != helper._hash_stream(incoming, helper.MAX_FILE_BYTES):
                    wired = False
    candidate = release.parent / "bootstrap-compose.yml"
    helper._atomic_file(candidate, data=overlay_data)
    _validate_compose(helper, core, config, runner, candidate, overlay is not None)
    return core, unit_path, unit, overlay, unit_data, overlay_data, socket_info, old, wired


def install(archive, config=None, sha256=None, runner=None, report=print, status_probe=None, system_root=Path("/")):
    """Core transaction; the CLI enforces root/Linux, tests inject host boundaries."""
    raw = config or InstallConfig()
    config = InstallConfig(**vars(raw))
    with verified_bundle(archive, sha256) as (helper, release, bundle):
        runner = runner or helper.SubprocessRunner()
        probe = status_probe or _status_probe
        system_root = Path(system_root)
        helper._safe_directory(system_root)
        preflight = _preflight(helper, release, bundle, config, runner, system_root)
        active = _systemctl(helper, runner, "is-active", query=True)
        enabled = _systemctl(helper, runner, "is-enabled", query=True)
        if active and preflight[2] is None:
            raise InstallError("An active service exists without the expected external unit; reconcile it before bootstrap.")
        _validate_loaded_unit(helper, runner, preflight[1] if preflight[2] is not None else None, required=active)
        stopped, result, record, state = False, None, None, None
        try:
            if preflight[2] is not None:
                _systemctl(helper, runner, "stop")
                stopped = True
            with _daemon_lock(helper, config.state_dir), helper.installation_lock(config.install_dir) as guard:
                # Repeat after draining and locking: a concurrent CLI/web update
                # may have changed the version or settings during preflight.
                core, unit_path, unit, overlay, unit_data, overlay_data, socket_info, old, wired = _preflight(
                    helper, release, bundle, config, runner, system_root)
                record, state = _record_settings(helper, config, unit, overlay, socket_info, active, enabled)
                helper._safe_directory(config.socket.parent, create=True, mode=0o750)
                os.chown(config.socket.parent, os.geteuid(), config.app_gid)
                config.socket.parent.chmod(0o750)
                inode = config.socket.parent.stat().st_ino
                if overlay is None:
                    helper._atomic_file(config.install_dir / OVERLAY, data=overlay_data, mode=0o644)
                state["phase"] = "settings_prepared"
                helper._atomic_file(record / "settings.json", data=(json.dumps(state, sort_keys=True) + "\n").encode())
                result = helper.upgrade(bundle.archive, core, sha256=bundle.sha256, runner=runner,
                                        report=report, reconfigure=not wired, _lock=guard)
                state["phase"] = "application_verified"
                state["backup"] = str(result.backup_dir) if result.backup_dir else None
                helper._atomic_file(record / "settings.json", data=(json.dumps(state, sort_keys=True) + "\n").encode())
                if unit is None:
                    helper._atomic_file(unit_path, data=unit_data, mode=0o644)
            # The new daemon needs both locks for startup/recovery. Every app
            # mutation above happened under one continuous installation lock.
            _systemctl(helper, runner, "daemon-reload")
            _validate_loaded_unit(helper, runner, unit_path, required=True)
            _systemctl(helper, runner, "enable")
            _systemctl(helper, runner, "start")
            status = probe(config.socket)
            if (not isinstance(status, dict) or type(status.get("protocol_version")) is not int
                    or status["protocol_version"] != 1
                    or not isinstance(status.get("phase"), str)
                    or status.get("phase") not in {"idle", "ready", "succeeded", "failed", "rolled_back"}
                    or not _systemctl(helper, runner, "is-active", query=True)):
                raise InstallError("The host updater did not return a usable protocol 1 status; inspect its retained operation state before retrying.")
            current = helper._running_app(core, runner)
            if (not current["healthy"] or (current["version"], current["commit"]) != (bundle.version, bundle.commit)
                    or not _runtime_wired(helper, config, runner, current)
                    or config.socket.parent.stat().st_ino != inode):
                raise InstallError("The current app did not pass version, health, environment and socket mount verification.")
            state["phase"] = "complete"
            helper._atomic_file(record / "settings.json", data=(json.dumps(state, sort_keys=True) + "\n").encode())
            report("Web updater installation complete: {} (commit {}). Recovery settings: {}".format(bundle.version, bundle.commit, record))
            return result
        except (InstallError, helper.UpgradeError, OSError, ValueError, UnicodeError, KeyboardInterrupt) as error:
            restored = False
            if result is None and record is not None:
                try:
                    with _daemon_lock(helper, config.state_dir), helper.installation_lock(config.install_dir):
                        current = helper._running_app(core, runner)
                        if current["healthy"] and all(current[k] == old[k] for k in ("version", "commit", "image_id")):
                            _restore_setting(helper, config.install_dir / OVERLAY, overlay)
                            _restore_setting(helper, unit_path, unit)
                            if socket_info is not None:
                                os.chown(config.socket.parent, socket_info["uid"], socket_info["gid"])
                                config.socket.parent.chmod(socket_info["mode"])
                            # Never remove a directory that an app might have bound.
                            restored = True
                except (OSError, InstallError, helper.UpgradeError):
                    pass
            if stopped and active and result is None and (record is None or restored):
                try:
                    _systemctl(helper, runner, "start")
                except (OSError, InstallError, helper.UpgradeError):
                    pass
            if record is not None:
                state["phase"] = "failed_settings_restored" if restored else "needs_retry_or_recovery"
                try:
                    helper._atomic_file(record / "settings.json", data=(json.dumps(state, sort_keys=True) + "\n").encode())
                except OSError:
                    pass
            reason = str(error) if isinstance(error, (InstallError, helper.UpgradeError)) else "Bootstrap was interrupted or a host operation failed."
            raise InstallError(reason + " Retry the same verified installer and archive after correcting the cause; retained backups and recovery settings: "
                               + str(record or config.state_dir) + ". No database restoration was attempted.") from None


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("--sha256")
    for name in ("install_dir", "updates_dir", "backups_dir", "state_dir", "socket"):
        parser.add_argument("--" + name.replace("_", "-"), type=Path, default=getattr(InstallConfig, name))
    parser.add_argument("--app-uid", type=int, default=1000)
    parser.add_argument("--app-gid", type=int, default=1000)
    args = parser.parse_args(argv)
    if os.geteuid() != 0 or sys.platform != "linux" or platform.machine() not in {"x86_64", "amd64"}:
        print("The web updater installer requires root on Linux x86_64.", file=sys.stderr)
        return 1
    previous = signal.getsignal(signal.SIGTERM)
    def interrupt(signum, frame):
        raise KeyboardInterrupt()
    try:
        signal.signal(signal.SIGTERM, interrupt)
        config = InstallConfig(**{key: getattr(args, key) for key in vars(InstallConfig())})
        install(args.archive, config, args.sha256)
        return 0
    except (InstallError, OSError, ValueError, UnicodeError, KeyboardInterrupt) as error:
        print("Web updater installation failed: " + (str(error) if isinstance(error, InstallError)
              else "Host operation failed or was interrupted; inspect retained backups and retry."), file=sys.stderr)
        return 1
    finally:
        signal.signal(signal.SIGTERM, previous)


if __name__ == "__main__":
    sys.exit(main())
