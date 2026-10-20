"""Real filesystem and Unix HTTP tests; no Docker, root, or network needed."""
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import signal
import socket
import stat
import struct
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import test_upgrade_car as fixture
import car_release
try:
    import car_update_service as service
except ModuleNotFoundError:
    service = None

VERSION, COMMIT = fixture.VERSION, fixture.COMMIT
OPERATION = "sysop-prepare.1:test"


class FakeRelease:
    def __init__(self, archive):
        self.archive = archive
        self.calls = []
        self.entered = threading.Event()
        self.proceed = threading.Event()
        self.proceed.set()
        self.fail = False

    def download_latest(self, version, directory):
        self.calls.append(version)
        self.entered.set()
        if not self.proceed.wait(5):
            raise AssertionError("Test release did not resume")
        if self.fail:
            raise car_release.ReleaseError("PRIVATE_TOKEN_MUST_NOT_APPEAR")
        target = Path(directory) / "download.tar.gz"
        with target.open("xb") as out:
            os.fchmod(out.fileno(), 0o600)
            out.write(self.archive.read_bytes())
        return car_release.DownloadedRelease(version, target, hashlib.sha256(target.read_bytes()).hexdigest())


class UpdaterCase(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(service, "The persistent Unix updater service must exist")
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.install = self.root / "install"
        for name, data in fixture.contents(fixture.OLD_VERSION, fixture.OLD_COMMIT).items():
            target = self.install / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        self.env = ("SUB2API_CAR_VERSION=" + fixture.OLD_VERSION + "\nSECRET=PRIVATE_PASSWORD_MUST_NOT_APPEAR\n").encode()
        (self.install / "deploy/.env").write_bytes(self.env)
        (self.install / "deploy/.env").chmod(0o600)
        app = self.install / "deploy/car-data/app"
        app.mkdir(parents=True)
        (app / "keep").write_bytes(b"app data")
        self.archive = fixture.make_bundle(self.root)
        self.config = service.ServiceConfig(install_dir=self.install, updates_dir=self.root / "updates",
                                             backups_dir=self.root / "backups", state_dir=self.root / "state")
        self.release = FakeRelease(self.archive)
        self.docker = fixture.FakeDocker(self.install)
        self.updater = self.make_service()
        self.addCleanup(lambda: self.updater.close())

    def make_service(self):
        return service.UpdaterService(self.config, release_client=self.release, runner=self.docker)

    def prepare(self):
        accepted = self.updater.prepare(OPERATION, VERSION)
        self.assertEqual(accepted["phase"], "preparing")
        self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.updater.status()["phase"], "ready")

    def activate(self):
        accepted = self.updater.activate(OPERATION)
        self.assertEqual(accepted["phase"], "applying")
        self.assertTrue(self.updater.wait_for_job(5))
        return self.updater.status()

    def use_current_migration_ledger(self):
        migrations = Path(__file__).resolve().parents[2] / "backend/migrations"
        ledger = {path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                  for path in sorted(migrations.glob("*.sql"))}
        self.assertGreaterEqual(len(ledger), 300, "Exercise the repository's full current migration history")
        files = fixture.contents()
        files["MIGRATIONS.json"] = json.dumps({"format_version": 1, "files": ledger}).encode()
        self.archive = fixture.make_bundle(self.root, files)
        self.docker.ledger = ledger
        return ledger

    def snapshot(self):
        return {p.relative_to(self.install).as_posix(): p.read_bytes()
                for p in self.install.rglob("*") if p.is_file() and p.name != ".upgrade-car.lock"}

class UpdaterTests(UpdaterCase):
    def test_prepare_verifies_image_without_live_mutation_backup_or_restart(self):
        before = self.snapshot()
        self.prepare()
        self.assertEqual(self.snapshot(), before)
        self.assertFalse(self.config.backups_dir.exists())
        self.assertEqual(self.release.calls, [VERSION])
        self.assertTrue(any("build" in c for c in self.docker.calls))
        self.assertTrue(any(c[:2] == ["docker", "run"] and self.docker.new_id in c for c in self.docker.calls))
        self.assertFalse(any(any(word in c for word in ("pg_dump", "pg_restore", "up", "stop")) for c in self.docker.calls))
        status = self.updater.status()
        self.assertEqual((status["protocol_version"], status["operation_id"], status["target_version"], status["target_commit"]),
                         (1, OPERATION, VERSION, COMMIT))
        self.assertNotIn("PRIVATE", json.dumps(status))

    def test_ready_persists_privately_and_resumes_without_downloading_or_replaying(self):
        self.prepare()
        expected = self.updater.status()
        self.updater.close()
        self.updater = self.make_service()
        self.assertEqual(self.updater.status(), expected)
        self.assertEqual(self.release.calls, [VERSION])
        self.assertEqual(stat.S_IMODE(self.config.state_dir.stat().st_mode), 0o700)
        for path in self.config.state_dir.rglob("*"):
            if path.is_file():
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertEqual(self.activate()["phase"], "succeeded")

    def test_activation_uses_pinned_archive_image_and_fresh_backup(self):
        self.prepare()
        self.archive.write_bytes(b"changed outside the pinned private staging")
        before_builds = len([c for c in self.docker.calls if "build" in c])
        status = self.activate()
        self.assertEqual(status["phase"], "succeeded")
        self.assertEqual(len([c for c in self.docker.calls if "build" in c]), before_builds)
        backup = Path(status["backup_path"])
        self.assertTrue((backup / "database.dump").is_file())
        self.assertEqual((backup / "files/deploy/.env").read_bytes(), self.env)
        self.assertTrue(self.updater.reload_requested.is_set())
        self.assertEqual(self.docker.version, VERSION)
        self.assertEqual(self.release.calls, [VERSION])

    def test_changed_prepared_archive_or_image_fails_before_mutation(self):
        for change in ("archive", "image"):
            with self.subTest(change=change):
                if change == "image":
                    self.updater.close()
                    self.config.state_dir = self.root / "state2"
                    self.updater = self.make_service()
                self.prepare()
                before = self.snapshot()
                if change == "archive":
                    state = json.loads((self.config.state_dir / "state.json").read_text())
                    Path(state["prepared"]["archive"]).write_bytes(b"tampered")
                else:
                    self.docker.new_id = "sha256:" + "f" * 64
                self.assertEqual(self.activate()["phase"], "failed")
                self.assertEqual(self.snapshot(), before)
                self.assertFalse(any("up" in c or "pg_dump" in c for c in self.docker.calls))

    def test_idempotency_reuses_same_operation_and_rejects_inconsistent_reuse(self):
        self.prepare()
        self.assertEqual(self.updater.prepare(OPERATION, VERSION)["phase"], "ready")
        with self.assertRaises(service.ServiceError) as raised:
            self.updater.prepare(OPERATION, "0.1.0-car.7")
        self.assertEqual(raised.exception.code, "OPERATION_CONFLICT")
        self.assertEqual(self.activate()["phase"], "succeeded")
        self.assertEqual(self.updater.activate(OPERATION)["phase"], "succeeded")
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)
        self.assertEqual(self.release.calls, [VERSION])

    def test_status_returns_during_prepare_and_rejects_concurrent_jobs(self):
        self.release.proceed.clear()
        self.addCleanup(self.release.proceed.set)
        accepted = self.updater.prepare(OPERATION, VERSION)
        self.assertEqual(accepted["phase"], "preparing")
        self.assertTrue(self.release.entered.wait(2))
        started = time.monotonic()
        self.assertEqual(self.updater.status()["phase"], "preparing")
        self.assertLess(time.monotonic() - started, 0.2)
        self.assertEqual(self.updater.prepare(OPERATION, VERSION)["phase"], "preparing")
        for action in (lambda: self.updater.prepare("another", VERSION), lambda: self.updater.activate(OPERATION)):
            with self.assertRaises(service.ServiceError):
                action()
        self.release.proceed.set()
        self.assertTrue(self.updater.wait_for_job(5))

    def test_ready_can_be_replaced_but_stale_activation_and_operation_reuse_are_rejected(self):
        self.prepare()
        self.updater.prepare("replacement", VERSION)
        self.assertTrue(self.updater.wait_for_job(5))
        with self.assertRaises(service.ServiceError) as raised:
            self.updater.activate(OPERATION)
        self.assertEqual(raised.exception.code, "STALE_OPERATION")
        self.assertNotEqual(self.updater.prepare(OPERATION, VERSION)["phase"], "ready")
        with self.assertRaises(service.ServiceError):
            self.updater.prepare(OPERATION, "0.1.0-car.7")
        self.assertEqual(len(self.release.calls), 2)

    def test_preparation_rejects_same_or_older_actual_running_version(self):
        self.docker.version, self.docker.commit = VERSION, COMMIT
        self.updater.prepare(OPERATION, VERSION)
        self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.updater.status()["error_code"], "VERSION_NOT_NEWER")
        self.assertFalse(any("build" in c for c in self.docker.calls))
        self.assertEqual(self.release.calls, [])

    def test_failed_download_and_image_verification_remain_safe_and_do_not_report_ready(self):
        self.release.fail = True
        before = self.snapshot()
        self.updater.prepare(OPERATION, VERSION)
        self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.updater.status()["phase"], "failed")
        self.assertNotIn("PRIVATE", json.dumps(self.updater.status()))
        self.release.fail = False
        self.docker.fail = "run"
        self.updater.prepare("next", VERSION)
        self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.updater.status()["phase"], "failed")
        self.assertEqual(self.snapshot(), before)
        self.assertFalse(any("up" in c or "pg_dump" in c for c in self.docker.calls))

    def test_verified_rollback_and_manual_recovery_map_from_helper_recovery_record(self):
        self.prepare()
        self.docker.fail_start = True
        status = self.activate()
        self.assertEqual(status["phase"], "rolled_back")
        self.assertEqual(json.loads((Path(status["backup_path"]) / "recovery.json").read_text())["status"], "rolled_back")
        self.assertEqual(self.docker.version, fixture.OLD_VERSION)
        self.assertFalse(self.updater.reload_requested.is_set())
        self.docker = fixture.FakeDocker(self.install)
        self.updater.close()
        self.updater = self.make_service()
        self.updater.prepare("migration", VERSION)
        self.assertTrue(self.updater.wait_for_job(5))
        self.docker.ledger = {}
        self.docker.fail_start = True
        self.updater.activate("migration")
        self.assertTrue(self.updater.wait_for_job(5))
        status = self.updater.status()
        self.assertEqual(status["phase"], "manual_recovery_required")
        self.assertTrue(Path(status["backup_path"]).is_dir())
        self.assertFalse(any("pg_restore" in c and "--list" not in c for c in self.docker.calls))
        with self.assertRaises(service.ServiceError):
            self.updater.prepare("must-not-retry", VERSION)

    def test_full_current_ledger_reports_the_helpers_verified_rollback(self):
        ledger = self.use_current_migration_ledger()
        self.prepare()
        self.docker.fail_start = True
        status = self.activate()
        records = list(self.config.backups_dir.glob("web-*/*/recovery.json"))
        self.assertEqual(len(records), 1)
        self.assertGreater(records[0].stat().st_size, service.MAX_STATE_BYTES)
        recovery = json.loads(records[0].read_text())
        self.assertEqual(recovery["status"], "rolled_back")
        self.assertEqual(recovery["migration_before"], ledger)
        self.assertEqual(recovery["migration_after"], ledger)
        self.assertEqual(status["phase"], "rolled_back")
        self.assertEqual(status["backup_path"], str(records[0].parent))
        self.assertEqual(self.docker.version, fixture.OLD_VERSION)

    def test_full_current_ledger_completed_restart_recovers_success_without_replay(self):
        ledger = self.use_current_migration_ledger()
        self.prepare()
        self.assertEqual(self.activate()["phase"], "succeeded")
        records = list(self.config.backups_dir.glob("web-*/*/recovery.json"))
        self.assertEqual(len(records), 1)
        self.assertGreater(records[0].stat().st_size, service.MAX_STATE_BYTES)
        recovery = json.loads(records[0].read_text())
        self.assertEqual(recovery["status"], "complete")
        self.assertEqual(recovery["migration_before"], ledger)
        self.assertEqual(recovery["migration_after"], ledger)
        self.updater.close()
        state_path = self.config.state_dir / "state.json"
        state = json.loads(state_path.read_text())
        state["status"]["phase"] = "applying"
        state_path.write_text(json.dumps(state))
        self.updater = self.make_service()
        self.assertEqual(self.updater.status()["phase"], "succeeded")
        self.assertEqual(self.updater.status()["backup_path"], str(records[0].parent))
        self.assertTrue(self.updater.reload_requested.is_set())
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)

    def test_recovery_record_has_an_independent_bound_and_control_state_stays_small(self):
        control = self.config.state_dir / "oversized-control.json"
        service.upgrade_car._atomic_file(control, data=json.dumps({"value": "x" * (64 * 1024)}).encode())
        with self.assertRaises(service.ServiceError):
            service._private_json(control)
        self.prepare()
        self.docker.fail_start = True
        with mock.patch.object(service, "MAX_RECOVERY_BYTES", 32, create=True):
            status = self.activate()
        records = list(self.config.backups_dir.glob("web-*/*/recovery.json"))
        self.assertEqual(json.loads(records[0].read_text())["status"], "rolled_back")
        self.assertEqual(status["phase"], "manual_recovery_required")

    def test_interrupted_preparation_is_failed_without_replay_and_interrupted_apply_is_manual(self):
        self.prepare()
        self.updater.close()
        state_file = self.config.state_dir / "state.json"
        state = json.loads(state_file.read_text())
        state["status"]["phase"] = "preparing"
        state_file.write_text(json.dumps(state))
        self.updater = self.make_service()
        self.assertEqual(self.updater.status()["phase"], "failed")
        self.assertEqual(self.updater.status()["error_code"], "OPERATION_INTERRUPTED")
        self.assertEqual(len(self.release.calls), 1)
        self.updater.close()
        state["status"]["phase"] = "applying"
        state["activated"] = True
        state_file.write_text(json.dumps(state))
        self.updater = self.make_service()
        self.assertEqual(self.updater.status()["phase"], "manual_recovery_required")
        self.assertEqual(len(self.release.calls), 1)
        self.assertFalse(any("up" in c for c in self.docker.calls))

    def test_restart_reconciles_completed_transaction_against_real_running_identity(self):
        self.prepare()
        self.assertEqual(self.activate()["phase"], "succeeded")
        self.updater.close()
        path = self.config.state_dir / "state.json"
        state = json.loads(path.read_text())
        state["status"]["phase"] = "applying"
        path.write_text(json.dumps(state))
        self.updater = self.make_service()
        self.assertEqual(self.updater.status()["phase"], "succeeded")
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)

    def test_only_one_service_process_owns_state_and_symlink_paths_are_refused(self):
        with self.assertRaises(service.ServiceError):
            self.make_service()
        self.updater.close()
        (self.config.state_dir / "state.json").unlink()
        outside = self.root / "outside"
        outside.write_text("private")
        (self.config.state_dir / "state.json").symlink_to(outside)
        with self.assertRaises(service.ServiceError):
            self.make_service()
        self.assertEqual(outside.read_text(), "private")

    def test_invalid_client_identifiers_and_versions_do_not_start_work(self):
        for operation in ("", "a" * 161, "../escape", "with space", "line\nbreak", 7):
            with self.subTest(operation=operation), self.assertRaises(service.ServiceError):
                self.updater.prepare(operation, VERSION)
        for version in ("latest", "v" + VERSION, "1.2.3", "1.2.3-car.1\n"):
            with self.subTest(version=version), self.assertRaises(service.ServiceError):
                self.updater.prepare(OPERATION, version)
        self.assertEqual(self.release.calls, [])

    def test_ready_is_published_only_after_the_installation_lock_is_released(self):
        reached, proceed = threading.Event(), threading.Event()
        self.addCleanup(proceed.set)
        original = service.upgrade_car.installation_lock
        @contextlib.contextmanager
        def delayed_unlock(path):
            with original(path):
                yield
                reached.set()
                if not proceed.wait(5):
                    raise AssertionError("Lock release did not resume")
        with mock.patch.object(service.upgrade_car, "installation_lock", delayed_unlock):
            self.updater.prepare(OPERATION, VERSION)
            self.assertTrue(reached.wait(2))
            try:
                self.assertEqual(self.updater.status()["phase"], "preparing")
            finally:
                proceed.set()
            self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.updater.status()["phase"], "ready")

    def test_successful_operation_remains_idempotent_while_waiting_for_daemon_reload(self):
        self.prepare()
        self.assertEqual(self.activate()["phase"], "succeeded")
        self.assertEqual(self.updater.prepare(OPERATION, VERSION)["phase"], "succeeded")
        self.assertEqual(self.updater.activate(OPERATION)["phase"], "succeeded")
        with self.assertRaises(service.ServiceError):
            self.updater.prepare("new-operation", VERSION)

    def test_persistence_failure_never_publishes_ready_or_accepts_another_job(self):
        original = service._save_json
        failing = []
        def disk_full(path, data):
            if (isinstance(data, dict) and data.get("status", {}).get("phase") == "ready") or failing:
                failing.append(True)
                raise OSError("simulated disk full")
            return original(path, data)
        with mock.patch.object(service, "_save_json", disk_full), mock.patch.object(threading, "excepthook"):
            self.updater.prepare(OPERATION, VERSION)
            self.assertTrue(self.updater.wait_for_job(5))
            self.assertEqual(self.updater.status()["phase"], "manual_recovery_required")
            self.assertEqual(self.updater.status()["error_code"], "STATE_INVALID")
            self.assertTrue(self.updater.stopping.is_set())
            with self.assertRaises(service.ServiceError):
                self.updater.prepare("another", VERSION)

    def test_unknown_recovery_record_is_manual_and_never_interpreted_from_error_text(self):
        self.prepare()
        def failed_upgrade(archive, config, **kwargs):
            backup = config.backups_dir / "before-test"
            backup.mkdir(parents=True, mode=0o700)
            record = {"status": "future-unknown-status", "target_version": VERSION, "target_commit": COMMIT}
            service.upgrade_car._atomic_file(backup / "recovery.json", data=json.dumps(record).encode())
            raise service.upgrade_car.UpgradeError("Application rolled back and verified healthy PRIVATE_SECRET")
        with mock.patch.object(service.upgrade_car, "upgrade", failed_upgrade):
            status = self.activate()
        self.assertEqual(status["phase"], "manual_recovery_required")
        self.assertNotIn("PRIVATE", json.dumps(status))

    def test_activation_continues_during_shutdown_without_accepting_duplicate_upgraders(self):
        self.prepare()
        entered, proceed = threading.Event(), threading.Event()
        self.addCleanup(proceed.set)
        docker = self.docker
        def paused_start(args, **kwargs):
            if "up" in args:
                entered.set()
                if not proceed.wait(5):
                    raise AssertionError("Activation did not resume")
            return docker(args, **kwargs)
        self.updater.runner = paused_start
        self.updater.activate(OPERATION)
        self.assertTrue(entered.wait(2))
        self.assertEqual(self.updater.status()["phase"], "applying")
        self.assertEqual(self.updater.activate(OPERATION)["phase"], "applying")
        with self.assertRaises(service.ServiceError):
            self.updater.prepare("concurrent", VERSION)
        closing = threading.Thread(target=self.updater.close)
        closing.start()
        self.assertTrue(self.updater.stopping.wait(2))
        try:
            self.assertTrue(closing.is_alive())
            with self.assertRaises(service.ServiceError):
                self.make_service()
        finally:
            proceed.set()
            closing.join(5)
        self.assertFalse(closing.is_alive())
        self.assertEqual(self.updater.status()["phase"], "succeeded")
        self.assertEqual(len([c for c in docker.calls if "up" in c]), 1)
        self.updater = self.make_service()
        self.assertEqual(self.updater.status()["phase"], "succeeded")

    def test_cli_termination_signal_drains_an_inflight_activation(self):
        self.prepare()
        entered, proceed = threading.Event(), threading.Event()
        self.addCleanup(proceed.set)
        docker = self.docker
        def paused_start(args, **kwargs):
            if "up" in args:
                entered.set()
                if not proceed.wait(5):
                    raise AssertionError("Activation did not resume")
            return docker(args, **kwargs)
        self.updater.runner = paused_start
        updater, observed = self.updater, []
        class FakeServer:
            def __init__(self, *args, **kwargs):
                pass
            def handle_request(self):
                updater.activate(OPERATION)
                if not entered.wait(2):
                    raise AssertionError("Activation did not start")
                signal.getsignal(signal.SIGTERM)(signal.SIGTERM, None)
                observed.append((updater.stopping.is_set(), updater.wait_for_job(0)))
                proceed.set()
            def server_close(self):
                pass
        previous = signal.getsignal(signal.SIGTERM)
        # The already-open fixture has its real UID; main's root/platform guard
        # is injected independently of its filesystem/transaction operations.
        real_geteuid = os.geteuid
        uid_calls = []
        def root_at_startup():
            uid_calls.append(True)
            return 0 if len(uid_calls) == 1 else real_geteuid()
        with mock.patch.object(service.os, "geteuid", root_at_startup), mock.patch.object(service.sys, "platform", "linux"), mock.patch.object(service.platform, "machine", return_value="x86_64"), mock.patch.object(service, "UpdaterService", return_value=updater), mock.patch.object(service, "UnixUpdateServer", FakeServer):
            self.assertEqual(service.main([]), 0)
        self.assertEqual(observed, [(True, False)])
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous)
        self.assertEqual(updater.status()["phase"], "succeeded")

    def test_transient_success_save_failure_reconciles_and_exits_for_reload(self):
        self.prepare()
        original = service._save_json
        failed, handles = [], []
        def fail_first_success(path, data):
            if not failed and isinstance(data, dict) and data.get("status", {}).get("phase") == "succeeded":
                failed.append(True)
                raise OSError("transient success persistence failure")
            return original(path, data)
        updater = self.updater
        class FakeServer:
            def __init__(self, *args, **kwargs):
                pass
            def handle_request(self):
                handles.append(True)
                if len(handles) != 1:
                    raise AssertionError("Successful recovery must trigger graceful daemon reload")
                updater.activate(OPERATION)
                if not updater.wait_for_job(5):
                    raise AssertionError("Activation did not finish")
            def server_close(self):
                pass
        real_geteuid, calls = os.geteuid, []
        def root_at_startup():
            calls.append(True)
            return 0 if len(calls) == 1 else real_geteuid()
        with mock.patch.object(service, "_save_json", fail_first_success), mock.patch.object(service.os, "geteuid", root_at_startup), mock.patch.object(service.sys, "platform", "linux"), mock.patch.object(service.platform, "machine", return_value="x86_64"), mock.patch.object(service, "UpdaterService", return_value=updater), mock.patch.object(service, "UnixUpdateServer", FakeServer):
            self.assertEqual(service.main([]), 0)
        self.assertEqual(len(failed), 1)
        self.assertEqual(len(handles), 1)
        self.assertEqual(updater.status()["phase"], "succeeded")
        self.assertTrue(updater.reload_requested.is_set())
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)
        self.updater = self.make_service()
        self.assertFalse(self.updater.stopping.is_set())
        self.assertEqual(self.updater.prepare("after-reload", "0.1.0-car.7")["phase"], "preparing")
        self.assertTrue(self.updater.wait_for_job(5))

    def test_safe_operation_characters_are_not_used_as_filesystem_paths(self):
        self.updater.prepare("..", VERSION)
        self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.updater.status()["phase"], "ready")
        records = list((self.config.state_dir / "operations").iterdir())
        self.assertEqual([p.name for p in records], [hashlib.sha256(b"..").hexdigest() + ".json"])

    def test_close_drains_current_job_and_rejects_new_work(self):
        self.release.proceed.clear()
        self.addCleanup(self.release.proceed.set)
        self.updater.prepare(OPERATION, VERSION)
        self.assertTrue(self.release.entered.wait(2))
        closing = threading.Thread(target=self.updater.close)
        closing.start()
        self.assertTrue(self.updater.stopping.wait(2))
        self.assertTrue(closing.is_alive())
        with self.assertRaises(service.ServiceError):
            self.updater.prepare("another", VERSION)
        self.release.proceed.set()
        closing.join(5)
        self.assertFalse(closing.is_alive())
        self.assertEqual(self.updater.status()["phase"], "ready")


class UnixHTTPTests(UpdaterCase):

    def start_server(self, peer=True):
        self.server = service.UnixUpdateServer(self.root / "control.sock", self.updater,
                                                app_uid=1000, app_gid=os.getgid(), peer_checker=lambda connection: peer)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01})
        self.thread.start()
        self.addCleanup(self.stop_server)

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)

    def request(self, method, path, body=None, raw_headers=None, send_request=True):
        data = b"" if body is None else json.dumps(body).encode()
        headers = raw_headers if raw_headers is not None else "Content-Type: application/json\r\nContent-Length: " + str(len(data)) + "\r\n"
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(3)
            connection.connect(str(self.root / "control.sock"))
            if send_request:
                connection.sendall((method + " " + path + " HTTP/1.1\r\nHost: localhost\r\n" + headers + "\r\n").encode() + data)
            response = bytearray()
            while True:
                piece = connection.recv(4096)
                if not piece:
                    break
                response.extend(piece)
        head, content = bytes(response).split(b"\r\n\r\n", 1)
        return int(head.split(b" ")[1]), json.loads(content)

    def test_http_direct_status_prepare_activate_and_exact_fields(self):
        self.start_server()
        code, status = self.request("GET", "/v1/status")
        self.assertEqual((code, status["protocol_version"], status["phase"]), (200, 1, "idle"))
        code, status = self.request("POST", "/v1/prepare", {"operation_id": OPERATION, "version": VERSION})
        self.assertEqual((code, status["phase"]), (202, "preparing"))
        self.assertTrue(self.updater.wait_for_job(5))
        code, status = self.request("POST", "/v1/activate", {"operation_id": OPERATION})
        self.assertEqual((code, status["phase"]), (202, "applying"))
        self.assertTrue(self.updater.wait_for_job(5))
        self.assertEqual(self.request("GET", "/v1/status")[1]["phase"], "succeeded")
        self.assertEqual(stat.S_IMODE((self.root / "control.sock").stat().st_mode), 0o660)

    def test_http_unknown_paths_methods_fields_oversized_and_ambiguous_bodies_rejected(self):
        self.start_server()
        cases = [("GET", "/v1/status?extra=1", None, None), ("DELETE", "/v1/status", None, None),
                 ("POST", "/v1/prepare", {"operation_id": OPERATION, "version": VERSION, "url": "https://evil"}, None),
                 ("POST", "/v1/prepare", {"operation_id": OPERATION}, None),
                 ("POST", "/v1/activate", {"operation_id": OPERATION, "version": VERSION}, None),
                 ("POST", "/v1/prepare", None, "Content-Length: 100000\r\n"),
                 ("POST", "/v1/prepare", None, "Transfer-Encoding: chunked\r\n"),
                 ("POST", "/v1/prepare", None, "Content-Length: 0\r\nContent-Length: 1\r\n")]
        for method, path, data, headers in cases:
            with self.subTest(method=method, path=path, headers=headers):
                code, error = self.request(method, path, data, headers)
                self.assertEqual(code, 400)
                self.assertEqual(set(error), {"error_code", "message"})
        self.assertEqual(self.release.calls, [])

    def test_http_peer_uid_is_checked_before_processing(self):
        self.start_server(peer=False)
        # Peer credentials are rejected before the server reads HTTP bytes.
        # Read that immediate rejection instead of racing a write with close.
        code, error = self.request("GET", "/v1/status", send_request=False)
        self.assertEqual(code, 503)
        self.assertEqual(error["error_code"], "PEER_NOT_ALLOWED")
        class Peer:
            def __init__(self, uid):
                self.uid = uid
            def getsockopt(self, *args):
                return struct.pack("3i", 42, self.uid, 1000)
        with mock.patch.object(service.sys, "platform", "linux"), mock.patch.object(service.socket, "SO_PEERCRED", 17, create=True):
            self.assertTrue(service.allowed_peer(Peer(0), 1000))
            self.assertTrue(service.allowed_peer(Peer(1000), 1000))
            self.assertFalse(service.allowed_peer(Peer(1001), 1000))

    def test_cli_requires_root_linux_and_has_no_unsafe_bypass(self):
        with mock.patch.object(service.os, "geteuid", return_value=501), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(service.main([]), 1)
        with mock.patch.object(service.os, "geteuid", return_value=0), mock.patch.object(service.sys, "platform", "darwin"), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(service.main([]), 1)


if __name__ == "__main__":
    unittest.main()
