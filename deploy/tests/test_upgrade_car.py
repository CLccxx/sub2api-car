"""Offline contract and transaction tests for the car server upgrader."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import os
import stat
import subprocess
import signal
import contextlib
from unittest import mock
import unittest

MODULE_PATH = Path(__file__).resolve().parents[1] / "upgrade_car.py"
SPEC = importlib.util.spec_from_file_location("upgrade_car", MODULE_PATH)
upgrader = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(upgrader)

VERSION = "0.1.0-car.6"
COMMIT = "a" * 40
OLD_VERSION = "0.1.0-car.5"
OLD_COMMIT = "b" * 40
SQL_HASH = "c" * 64


def contents(version=VERSION, commit=COMMIT, catalog=True):
    files = {
        "sub2api": b"binary", "BUILD_VERSION": (version + "\n").encode(),
        "BUILD_COMMIT": (commit + "\n").encode(),
        "Dockerfile.goreleaser": b"FROM scratch\n", ".dockerignore": b".env*\n",
        "backend/resources/default.json": b"{}\n",
        "deploy/docker-entrypoint.sh": b"#!/bin/sh\nexec \"$@\"\n",
        "deploy/docker-compose.car.yml": b"incoming compose\n",
        "deploy/Caddyfile.car": b"incoming caddy\n",
        "deploy/init-car.sh": b"#!/bin/sh\n",
        "deploy/CARPOOL_DEPLOYMENT.md": b"deployment docs\n",
    }
    if catalog:
        files["MIGRATIONS.json"] = json.dumps({"format_version": 1, "files": {"001_init.sql": SQL_HASH}}).encode()
    return files


def make_bundle(directory, files=None, extra=None, omitted=(), manifest_override=None, pax_headers=None):
    files = dict(contents() if files is None else files)
    manifest = "".join(hashlib.sha256(data).hexdigest() + "  " + name + "\n"
                       for name, data in sorted(files.items()) if name not in omitted)
    files["SHA256SUMS"] = manifest.encode() if manifest_override is None else manifest_override
    archive = Path(directory) / "release.tar.gz"
    with tarfile.open(archive, "w:gz") as tar:
        for name, data in files.items():
            item = tarfile.TarInfo("sub2api-car/" + name)
            item.size = len(data)
            if name == "sub2api" and pax_headers:
                item.pax_headers = pax_headers
            item.mode = 0o755 if name == "sub2api" or name.endswith(".sh") else 0o644
            tar.addfile(item, io.BytesIO(data))
        if extra:
            item, data = extra
            tar.addfile(item, io.BytesIO(data) if data is not None else None)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    Path(str(archive) + ".sha256").write_text(digest + "  " + archive.name + "\n")
    return archive


class BundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_check_only_verifies_manifest_without_docker_or_installation(self):
        archive = make_bundle(self.root)
        bundle = upgrader.verify_archive(archive)
        self.assertEqual((bundle.version, bundle.commit), (VERSION, COMMIT))
        self.assertEqual(bundle.migrations, {"001_init.sql": SQL_HASH})
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(upgrader.main([str(archive), "--check-only"]), 0)

    def test_corrupt_outer_checksum_and_wrong_sidecar_basename_are_rejected(self):
        archive = make_bundle(self.root)
        with self.assertRaises(upgrader.UpgradeError):
            upgrader.verify_archive(archive, "0" * 64)
        Path(str(archive) + ".sha256").write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + "  other.tar.gz\n")
        with self.assertRaises(upgrader.UpgradeError):
            upgrader.verify_archive(archive)

    def test_archive_rejects_unsafe_duplicate_and_nonregular_members(self):
        for name, kind in [("sub2api-car/../escape", tarfile.REGTYPE),
                           ("/sub2api-car/sub2api", tarfile.REGTYPE),
                           ("sub2api-car//evil", tarfile.REGTYPE),
                           ("sub2api-car/sub2api", tarfile.REGTYPE),
                           ("sub2api-car/backend/resources/link", tarfile.SYMTYPE),
                           ("sub2api-car/backend/resources/link", tarfile.LNKTYPE),
                           ("sub2api-car/backend/resources/device", tarfile.CHRTYPE)]:
            with self.subTest(name=name, kind=kind):
                item = tarfile.TarInfo(name)
                item.type = kind
                item.linkname = "/etc/passwd" if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE) else ""
                archive = make_bundle(self.root, extra=(item, b"" if kind == tarfile.REGTYPE else None))
                with self.assertRaises(upgrader.UpgradeError):
                    upgrader.verify_archive(archive)

    def test_unlisted_altered_and_secret_files_are_rejected(self):
        for change in ("unlisted", "altered", ".env", "car-data"):
            with self.subTest(change=change):
                files = contents()
                kwargs = {}
                if change == "unlisted":
                    kwargs["omitted"] = ("sub2api",)
                elif change == "altered":
                    kwargs["manifest_override"] = ("0" * 64 + "  sub2api\n").encode()
                else:
                    files["backend/resources/" + change + "/secret"] = b"secret"
                with self.assertRaises(upgrader.UpgradeError):
                    upgrader.verify_archive(make_bundle(self.root, files, **kwargs))

    def test_invalid_version_commit_and_migration_metadata_are_rejected(self):
        cases = [contents(version="latest"), contents(version="1.2.3-car.4\nINJECT=1"),
                 contents(commit="unknown"), contents(commit="A" * 40)]
        for metadata in ({"format_version": 2, "files": {}}, {"format_version": True, "files": {}},
                         {"format_version": 1, "files": {"../001.sql": SQL_HASH}},
                         {"format_version": 1, "files": {"001.sql": "bad"}},
                         {"format_version": 1, "files": {}}, {"format_version": 1, "files": [], "extra": 1}):
            files = contents()
            files["MIGRATIONS.json"] = json.dumps(metadata).encode()
            cases.append(files)
        for files in cases:
            with self.subTest(files=files.get("MIGRATIONS.json")):
                with self.assertRaises(upgrader.UpgradeError):
                    upgrader.verify_archive(make_bundle(self.root, files))

    def test_conflicting_parent_file_and_archive_limits_are_rejected(self):
        files = contents()
        files["backend/resources/default.json/child"] = b"conflict"
        with self.assertRaises(upgrader.UpgradeError):
            upgrader.verify_archive(make_bundle(self.root, files))
        archive = make_bundle(self.root)
        for limit in ("MAX_MEMBERS", "MAX_ARCHIVE_BYTES", "MAX_EXPANDED_BYTES", "MAX_FILE_BYTES", "MAX_METADATA_BYTES"):
            with self.subTest(limit=limit), mock.patch.object(upgrader, limit, 1):
                with self.assertRaises(upgrader.UpgradeError):
                    upgrader.verify_archive(archive)

    def test_tar_extension_metadata_is_bounded_before_tarfile_allocates_it(self):
        archive = make_bundle(self.root, pax_headers={"comment": "x" * 128})
        with mock.patch.object(upgrader, "MAX_TAR_METADATA_BYTES", 64, create=True):
            with self.assertRaises(upgrader.UpgradeError):
                upgrader.verify_archive(archive)

    def test_legacy_bundle_can_be_checked_without_migration_catalog(self):
        bundle = upgrader.verify_archive(make_bundle(self.root, contents(catalog=False)))
        self.assertIsNone(bundle.migrations)

    def test_web_updater_bundle_files_are_optional_for_legacy_and_allowed(self):
        files = contents()
        for name in ("car_release.py", "car_update_service.py", "install_car_web_update.py",
                     "sub2api-car-updater.service", "docker-compose.car-updater.yml"):
            files["deploy/" + name] = b"web updater fixture\n"
        bundle = upgrader.verify_archive(make_bundle(self.root, files))
        self.assertIn("deploy/install_car_web_update.py", bundle.files)


class FakeDocker:
    """A command boundary simulator; all filesystem transactions stay real."""
    def __init__(self, install):
        self.install = install
        self.calls = []
        self.version = OLD_VERSION
        self.commit = OLD_COMMIT
        self.health = "healthy"
        self.fail = None
        self.fail_start = False
        self.mismatch_running = False
        self.interrupt_start = False
        self.fail_rollback_start = False
        self.interrupt_rollback_start = False
        self.ledger = {"001_init.sql": SQL_HASH}
        self.ledger_after = None
        self.switches = 0
        self.new_id = "sha256:" + "d" * 64
        self.old_id = "sha256:" + "e" * 64

    def __call__(self, args, cwd=None, stdin=None, stdout=None, timeout=None):
        args = list(args)
        self.calls.append(args)
        output = b""
        code = 0
        if self.fail and self.fail in args:
            code = 9
        elif args[:3] == ["docker", "image", "inspect"]:
            output = (self.new_id + "\n").encode()
        elif args[:2] == ["docker", "inspect"]:
            image = self.old_id if self.version == OLD_VERSION else self.new_id
            output = (image + "\tsub2api-car:" + self.version + "\trunning\t" + self.health + "\n").encode()
        elif args[:2] == ["docker", "exec"]:
            commit = "f" * 40 if self.mismatch_running and self.version == VERSION else self.commit
            output = ("Sub2API " + self.version + " (commit: " + commit + ", built: now)\n").encode()
        elif args[:2] == ["docker", "run"]:
            output = ("Sub2API " + VERSION + " (commit: " + COMMIT + ", built: now)\n").encode()
        elif "ps" in args:
            output = b"container123\n"
        elif "psql" in args:
            ledger = self.ledger_after if self.switches and self.ledger_after is not None else self.ledger
            if ledger is None:
                code = 1
            else:
                output = "".join(n + "\t" + h + "\n" for n, h in sorted(ledger.items())).encode()
        elif "pg_dump" in args:
            if stdout is None:
                raise AssertionError("database dumps must stream to a private file")
            stdout.write(b"PGDMPfake")
        elif "pg_restore" in args:
            if "--list" not in args:
                raise AssertionError("automatic database restoration is forbidden")
            if stdin.read() != b"PGDMPfake":
                raise AssertionError("pg_restore must validate the saved dump")
        elif "up" in args:
            self.switches += 1
            env = (self.install / "deploy/.env").read_bytes()
            self.version = VERSION if VERSION.encode() in env else OLD_VERSION
            self.commit = COMMIT if self.version == VERSION else OLD_COMMIT
            if self.version == VERSION and self.interrupt_start:
                raise KeyboardInterrupt()
            if self.version == VERSION and self.fail_start:
                code = 7
            if self.version == OLD_VERSION and self.fail_rollback_start:
                code = 8
            if self.version == OLD_VERSION and self.interrupt_rollback_start:
                raise KeyboardInterrupt()
        if stdout is not None and output:
            stdout.write(output)
            output = b""
        return subprocess.CompletedProcess(args, code, output, b"PRIVATE_PASSWORD_MUST_NOT_APPEAR")


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        # macOS /var is a symlink; use the canonical temporary root.
        self.root = Path(self.temp.name).resolve()
        self.install = self.root / "installed"
        for name, data in contents(OLD_VERSION, OLD_COMMIT).items():
            path = self.install / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        self.env = b"# secrets stay verbatim\r\nSUB2API_CAR_VERSION = '0.1.0-car.5'  # release\r\nPASSWORD='do not source $(touch injected)'\r\nJWT_SECRET=unchanged\r\n"
        (self.install / "deploy/.env").write_bytes(self.env)
        (self.install / "deploy/.env").chmod(0o600)
        (self.install / "deploy/docker-compose.car.yml").write_bytes(b"operator compose\n")
        (self.install / "deploy/docker-compose.car.yml").chmod(0o640)
        (self.install / "deploy/Caddyfile.car").write_bytes(b"operator caddy\n")
        (self.install / "deploy/Caddyfile.car").chmod(0o640)
        (self.install / "backend/resources/obsolete.json").write_bytes(b"old resource")
        for directory in ("app", "postgres", "redis", "caddy-data", "caddy-config"):
            path = self.install / "deploy/car-data" / directory
            path.mkdir(parents=True)
            (path / "keep").write_bytes(directory.encode())
        self.archive = make_bundle(self.root)
        self.config = upgrader.UpgradeConfig(self.install, self.root / "updates", self.root / "backups")
        self.docker = FakeDocker(self.install)
        self.reports = []

    def run_upgrade(self):
        return upgrader.upgrade(self.archive, self.config, runner=self.docker, report=self.reports.append)

    def test_success_changes_only_app_and_preserves_operator_files_secrets_and_data(self):
        result = self.run_upgrade()
        self.assertEqual((result.version, result.commit, result.no_op), (VERSION, COMMIT, False))
        self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env.replace(OLD_VERSION.encode(), VERSION.encode()))
        self.assertEqual(stat.S_IMODE((self.install / "deploy/.env").stat().st_mode), 0o600)
        for name in ("docker-compose.car.yml", "Caddyfile.car"):
            path = self.install / "deploy" / name
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o640)
            self.assertTrue(path.read_bytes().startswith(b"operator"))
        for directory in ("app", "postgres", "redis", "caddy-data", "caddy-config"):
            self.assertEqual((self.install / "deploy/car-data" / directory / "keep").read_bytes(), directory.encode())
        self.assertEqual((result.backup_dir / "files/deploy/.env").read_bytes(), self.env)
        self.assertEqual((result.backup_dir / "app/keep").read_bytes(), b"app")
        self.assertEqual(stat.S_IMODE(result.backup_dir.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE((result.backup_dir / "database.dump").stat().st_mode), 0o600)
        self.assertFalse((self.root / "injected").exists())
        up = [c for c in self.docker.calls if "up" in c]
        self.assertEqual(len(up), 1)
        self.assertEqual(up[0][up[0].index("up"):], ["up", "-d", "--no-deps", "--no-build", "--pull", "never", "--timeout", "30", "--wait", "--wait-timeout", "120", "sub2api"])
        self.assertEqual(up[0][-1], "sub2api")
        self.assertFalse(any("stop" in c for c in self.docker.calls))
        self.assertNotIn("PRIVATE_PASSWORD", " ".join(self.reports))

    def test_prepared_image_reuses_verified_immutable_image_and_still_takes_backup(self):
        self.assertTrue(hasattr(upgrader, "PreparedImage"), "Explicit prepared image contract is required")
        verified = upgrader.verify_archive(self.archive)
        prepared = upgrader.PreparedImage(verified.sha256, VERSION, COMMIT, self.docker.new_id)
        result = upgrader.upgrade(self.archive, self.config, runner=self.docker,
                                  report=self.reports.append, prepared_image=prepared)
        self.assertFalse(any("build" in call for call in self.docker.calls))
        self.assertTrue((result.backup_dir / "database.dump").is_file())
        self.assertTrue(any(call[:2] == ["docker", "run"] and prepared.image_id in call for call in self.docker.calls))
        self.assertEqual((result.version, result.commit), (VERSION, COMMIT))

    def test_prepared_image_mismatches_leave_live_files_and_containers_untouched(self):
        self.assertTrue(hasattr(upgrader, "PreparedImage"), "Explicit prepared image contract is required")
        checksum = upgrader.verify_archive(self.archive).sha256
        for mismatch in ("archive", "version", "commit", "image_id", "tag", "runtime"):
            with self.subTest(mismatch=mismatch):
                self.docker = FakeDocker(self.install)
                values = [checksum, VERSION, COMMIT, self.docker.new_id]
                if mismatch == "archive":
                    values[0] = "f" * 64
                elif mismatch == "version":
                    values[1] = OLD_VERSION
                elif mismatch == "commit":
                    values[2] = OLD_COMMIT
                elif mismatch == "image_id":
                    values[3] = "invalid"
                elif mismatch == "tag":
                    self.docker.new_id = "sha256:" + "f" * 64
                runner = self.docker
                if mismatch == "runtime":
                    def wrong_runtime(args, **kwargs):
                        result = self.docker(args, **kwargs)
                        if args[:2] == ["docker", "run"]:
                            result.stdout = ("Sub2API " + OLD_VERSION + " (commit: " + OLD_COMMIT + ", built: now)\n").encode()
                        return result
                    runner = wrong_runtime
                with self.assertRaises(upgrader.UpgradeError):
                    upgrader.upgrade(self.archive, self.config, runner=runner, report=self.reports.append,
                                      prepared_image=upgrader.PreparedImage(*values))
                self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
                self.assertEqual((self.install / "BUILD_VERSION").read_text().strip(), OLD_VERSION)
                self.assertFalse(any("build" in c or "up" in c or "stop" in c or "pg_dump" in c for c in self.docker.calls))

    def test_prepared_image_cannot_downgrade_or_accept_same_version_with_different_image(self):
        checksum = upgrader.verify_archive(self.archive).sha256
        prepared = upgrader.PreparedImage(checksum, VERSION, COMMIT, self.docker.new_id)
        for running_version in ("0.1.0-car.7", VERSION):
            with self.subTest(running_version=running_version):
                self.docker = FakeDocker(self.install)
                self.docker.version, self.docker.commit = running_version, COMMIT
                if running_version == VERSION:
                    self.docker.new_id = "sha256:" + "f" * 64
                env = self.env.replace(OLD_VERSION.encode(), running_version.encode())
                (self.install / "deploy/.env").write_bytes(env)
                with self.assertRaises(upgrader.UpgradeError):
                    upgrader.upgrade(self.archive, self.config, runner=self.docker, report=self.reports.append,
                                      prepared_image=prepared)
                self.assertEqual((self.install / "deploy/.env").read_bytes(), env)
                self.assertFalse(any("build" in c or "up" in c or "pg_dump" in c for c in self.docker.calls))

    def test_prepared_noop_revalidates_tag_identity_and_immutable_image_version(self):
        checksum = upgrader.verify_archive(self.archive).sha256
        self.docker.version, self.docker.commit = VERSION, COMMIT
        env = self.env.replace(OLD_VERSION.encode(), VERSION.encode())
        (self.install / "deploy/.env").write_bytes(env)
        prepared = upgrader.PreparedImage(checksum, VERSION, COMMIT, self.docker.new_id)
        for mismatch in ("tag", "runtime", None):
            with self.subTest(mismatch=mismatch):
                self.docker.calls.clear()
                def inspect_candidate(args, **kwargs):
                    result = self.docker(args, **kwargs)
                    if mismatch == "tag" and args[:3] == ["docker", "image", "inspect"]:
                        result.stdout = ("sha256:" + "f" * 64 + "\n").encode()
                    if mismatch == "runtime" and args[:2] == ["docker", "run"]:
                        result.stdout = ("Sub2API " + OLD_VERSION + " (commit: " + OLD_COMMIT + ", built: now)\n").encode()
                    return result
                if mismatch is not None:
                    with self.assertRaises(upgrader.UpgradeError):
                        upgrader.upgrade(self.archive, self.config, runner=inspect_candidate,
                                         report=self.reports.append, prepared_image=prepared)
                else:
                    result = upgrader.upgrade(self.archive, self.config, runner=inspect_candidate,
                                              report=self.reports.append, prepared_image=prepared)
                    self.assertTrue(result.no_op)
                    self.assertTrue(any(c[:2] == ["docker", "run"] and prepared.image_id in c for c in self.docker.calls))
                self.assertEqual((self.install / "deploy/.env").read_bytes(), env)
                self.assertFalse(any("build" in c or "up" in c or "pg_dump" in c for c in self.docker.calls))

    def test_build_backup_and_image_verification_failures_leave_live_version_intact(self):
        for stage in ("build", "pg_dump", "pg_restore", "run"):
            with self.subTest(stage=stage):
                self.docker = FakeDocker(self.install)
                self.docker.fail = stage
                with self.assertRaises(upgrader.UpgradeError) as raised:
                    self.run_upgrade()
                self.assertNotIn("PRIVATE_PASSWORD", str(raised.exception))
                self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
                self.assertEqual((self.install / "BUILD_VERSION").read_text().strip(), OLD_VERSION)
                self.assertFalse(any("up" in c or "stop" in c for c in self.docker.calls))

    def test_failed_start_and_runtime_version_mismatch_roll_back_unchanged_schema(self):
        for flag in ("fail_start", "mismatch_running", "interrupt_start"):
            with self.subTest(flag=flag):
                self.docker = FakeDocker(self.install)
                setattr(self.docker, flag, True)
                with self.assertRaisesRegex(upgrader.UpgradeError, "rolled back"):
                    self.run_upgrade()
                self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
                self.assertEqual((self.install / "BUILD_VERSION").read_text().strip(), OLD_VERSION)
                self.assertEqual((self.install / "backend/resources/obsolete.json").read_bytes(), b"old resource")
                self.assertEqual(self.docker.version, OLD_VERSION)
                self.assertTrue(any("stop" in c and c[-1] == "sub2api" for c in self.docker.calls))
                for call in self.docker.calls:
                    if "up" in call or "stop" in call:
                        self.assertEqual(call[-1], "sub2api")

    def test_pending_changed_unknown_and_changed_after_migrations_never_auto_rollback(self):
        for condition in ("pending", "changed", "unknown", "legacy", "changed_after"):
            with self.subTest(condition=condition):
                self.docker = FakeDocker(self.install)
                self.docker.fail_start = True
                (self.install / "deploy/.env").write_bytes(self.env)
                (self.install / "BUILD_VERSION").write_text(OLD_VERSION + "\n")
                (self.install / "BUILD_COMMIT").write_text(OLD_COMMIT + "\n")
                self.archive = make_bundle(self.root, contents(catalog=condition != "legacy"))
                if condition == "pending":
                    self.docker.ledger = {}
                elif condition == "changed":
                    self.docker.ledger = {"001_init.sql": "f" * 64}
                elif condition == "unknown":
                    self.docker.ledger = None
                elif condition == "changed_after":
                    self.docker.ledger_after = {"001_init.sql": SQL_HASH, "002_new.sql": SQL_HASH}
                with self.assertRaisesRegex(upgrader.UpgradeError, "manual recovery"):
                    self.run_upgrade()
                self.assertEqual((self.install / "BUILD_VERSION").read_text().strip(), VERSION)
                self.assertEqual(self.docker.switches, 1)
                self.assertFalse(any("tag" in c for c in self.docker.calls))
                self.assertTrue(any("stop" in c for c in self.docker.calls))
                self.assertFalse(any("pg_restore" in c and "--list" not in c for c in self.docker.calls))

    def test_lock_contention_prevents_docker_actions(self):
        with upgrader.installation_lock(self.install):
            with self.assertRaisesRegex(upgrader.UpgradeError, "another upgrade"):
                self.run_upgrade()
        self.assertEqual(self.docker.calls, [])

    def test_same_running_version_requires_same_commit_and_health_for_noop(self):
        self.docker.version, self.docker.commit = VERSION, COMMIT
        (self.install / "deploy/.env").write_bytes(self.env.replace(OLD_VERSION.encode(), VERSION.encode()))
        (self.install / "BUILD_VERSION").write_text(VERSION + "\n")
        (self.install / "BUILD_COMMIT").write_text(COMMIT + "\n")
        self.assertTrue(self.run_upgrade().no_op)
        self.assertFalse(any("build" in c or "up" in c for c in self.docker.calls))
        self.docker.health = "unhealthy"
        with self.assertRaises(upgrader.UpgradeError):
            self.run_upgrade()
        self.docker.health, self.docker.commit = "healthy", OLD_COMMIT
        with self.assertRaises(upgrader.UpgradeError):
            self.run_upgrade()
        self.assertFalse(any("build" in c for c in self.docker.calls))

    def test_invalid_bundle_never_invokes_docker(self):
        Path(str(self.archive) + ".sha256").write_text("0" * 64 + "  " + self.archive.name + "\n")
        with self.assertRaises(upgrader.UpgradeError):
            self.run_upgrade()
        self.assertEqual(self.docker.calls, [])

    def test_overlay_is_optional_retained_and_used_for_every_compose_call(self):
        overlay = self.install / "deploy/docker-compose.car-updater.yml"
        overlay.write_bytes(b"operator socket override\n")
        overlay.chmod(0o640)
        result = self.run_upgrade()
        self.assertEqual(overlay.read_bytes(), b"operator socket override\n")
        self.assertEqual(stat.S_IMODE(overlay.stat().st_mode), 0o640)
        self.assertEqual((result.backup_dir / "files/deploy/docker-compose.car-updater.yml").read_bytes(), overlay.read_bytes())
        for call in self.docker.calls:
            if call[:2] == ["docker", "compose"]:
                self.assertIn(str(overlay), call)
                self.assertEqual(call[call.index(str(overlay)) - 1], "-f")

    def test_overlay_symlinks_rejected_before_docker(self):
        (self.install / "deploy/docker-compose.car-updater.yml").symlink_to(self.install / "deploy/docker-compose.car.yml")
        with self.assertRaises(upgrader.UpgradeError):
            self.run_upgrade()
        self.assertEqual(self.docker.calls, [])

    def test_same_version_reconfigure_uses_backup_and_rejects_conflict_or_unhealthy(self):
        self.docker.version, self.docker.commit = VERSION, COMMIT
        (self.install / "deploy/.env").write_bytes(self.env.replace(OLD_VERSION.encode(), VERSION.encode()))
        (self.install / "BUILD_VERSION").write_text(VERSION + "\n")
        (self.install / "BUILD_COMMIT").write_text(COMMIT + "\n")
        for commit, health in ((OLD_COMMIT, "healthy"), (COMMIT, "unhealthy")):
            self.docker.commit, self.docker.health = commit, health
            with self.assertRaises(upgrader.UpgradeError):
                upgrader.upgrade(self.archive, self.config, runner=self.docker, report=self.reports.append, reconfigure=True)
        self.docker.commit, self.docker.health = COMMIT, "healthy"
        result = upgrader.upgrade(self.archive, self.config, runner=self.docker, report=self.reports.append, reconfigure=True)
        self.assertFalse(result.no_op)
        self.assertIsNotNone(result.backup_dir)
        self.assertTrue((result.backup_dir / "database.dump").exists())
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)
        self.assertIn("--force-recreate", next(c for c in self.docker.calls if "up" in c))
        self.assertFalse(any("build" in c for c in self.docker.calls))

    def test_installer_lock_handoff_cannot_outlive_lock(self):
        with upgrader.installation_lock(self.install) as guard:
            result = upgrader.upgrade(self.archive, self.config, runner=self.docker, report=self.reports.append, _lock=guard)
        self.assertFalse(result.no_op)
        with self.assertRaises(upgrader.UpgradeError):
            upgrader.upgrade(self.archive, self.config, runner=self.docker, report=self.reports.append, _lock=guard)

    def test_symlink_live_target_and_lock_are_rejected(self):
        victim = self.root / "outside"
        victim.write_bytes(b"private")
        target = self.install / ".upgrade-car.lock"
        target.symlink_to(victim)
        with self.assertRaises(upgrader.UpgradeError):
            self.run_upgrade()
        self.assertEqual(victim.read_bytes(), b"private")
        target.unlink()
        (self.install / "sub2api").unlink()
        (self.install / "sub2api").symlink_to(victim)
        with self.assertRaises(upgrader.UpgradeError):
            self.run_upgrade()
        self.assertEqual(victim.read_bytes(), b"private")
        self.assertFalse(any("up" in c for c in self.docker.calls))

    def test_new_nested_managed_files_are_installed_and_removed_on_rollback(self):
        files = contents()
        files["backend/resources/nested/new.json"] = b"new"
        files["deploy/upgrade_car.py"] = b"# new helper\n"
        self.archive = make_bundle(self.root, files)
        self.docker.fail_start = True
        with self.assertRaisesRegex(upgrader.UpgradeError, "rolled back"):
            self.run_upgrade()
        self.assertFalse((self.install / "backend/resources/nested/new.json").exists())
        self.assertFalse((self.install / "deploy/upgrade_car.py").exists())
        self.docker = FakeDocker(self.install)
        self.run_upgrade()
        self.assertEqual((self.install / "backend/resources/nested/new.json").read_bytes(), b"new")

    def test_storage_directories_cannot_overlap_the_installation(self):
        self.config.backups_dir = self.install / "deploy/car-data/app/backups"
        with self.assertRaisesRegex(upgrader.UpgradeError, "overlap"):
            self.run_upgrade()
        self.assertEqual(self.docker.calls, [])

    def test_pending_migration_and_file_copy_failure_restore_files_without_restarting_old_app(self):
        self.docker.ledger = {}
        original = upgrader._atomic_file
        failed = []
        def fail_live_copy(target, *args, **kwargs):
            if target == self.install / "sub2api" and not failed:
                failed.append(True)
                raise OSError("simulated write failure")
            return original(target, *args, **kwargs)
        with mock.patch.object(upgrader, "_atomic_file", side_effect=fail_live_copy):
            with self.assertRaisesRegex(upgrader.UpgradeError, "previous app kept running"):
                self.run_upgrade()
        self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
        self.assertEqual((self.install / "BUILD_VERSION").read_text().strip(), OLD_VERSION)
        self.assertEqual((self.install / "backend/resources/obsolete.json").read_bytes(), b"old resource")
        self.assertFalse(any("up" in c or "stop" in c or "tag" in c for c in self.docker.calls))
        self.assertEqual(self.docker.version, OLD_VERSION)

    def test_failed_rollback_restart_stops_app_and_requires_manual_recovery(self):
        self.docker.fail_start = True
        self.docker.fail_rollback_start = True
        with self.assertRaisesRegex(upgrader.UpgradeError, "manual recovery"):
            self.run_upgrade()
        self.assertEqual(len([c for c in self.docker.calls if "stop" in c]), 2)
        self.assertFalse(any("pg_restore" in c and "--list" not in c for c in self.docker.calls))

    def test_interrupted_rollback_restart_stops_app_and_records_manual_recovery(self):
        self.docker.fail_start = True
        self.docker.interrupt_rollback_start = True
        with self.assertRaisesRegex(upgrader.UpgradeError, "manual recovery"):
            self.run_upgrade()
        self.assertEqual(len([c for c in self.docker.calls if "stop" in c]), 2)
        records = list(self.config.backups_dir.glob("*/recovery.json"))
        self.assertEqual(len(records), 1)
        state = json.loads(records[0].read_text())
        self.assertEqual(state["status"], "manual_recovery_required")
        self.assertIn("interrupted", state["recovery_error"])

    def test_existing_safe_0755_update_and_backup_parents_are_made_private(self):
        self.config.updates_dir.mkdir(mode=0o755)
        self.config.backups_dir.mkdir(mode=0o755)
        result = self.run_upgrade()
        for directory in (self.config.updates_dir, self.config.backups_dir, result.stage_dir, result.backup_dir):
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)

    def test_termination_signal_is_handled_during_actual_upgrade(self):
        observed = []
        previous = signal.getsignal(signal.SIGTERM)
        def interrupted(*args):
            handler = signal.getsignal(signal.SIGTERM)
            observed.append(callable(handler))
            if callable(handler):
                handler(signal.SIGTERM, None)
        with mock.patch.object(upgrader.os, "geteuid", return_value=0), mock.patch.object(upgrader, "upgrade", side_effect=interrupted):
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(upgrader.main([str(self.archive)]), 1)
        self.assertEqual(observed, [True])
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous)

    def test_cli_requires_root_only_for_actual_upgrade(self):
        with mock.patch.object(upgrader.os, "geteuid", return_value=501):
            errors = io.StringIO()
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(errors):
                self.assertEqual(upgrader.main([str(self.archive), "--check-only"]), 0)
                self.assertEqual(upgrader.main([str(self.archive)]), 1)
            self.assertIn("requires root", errors.getvalue())


if __name__ == "__main__":
    unittest.main()
