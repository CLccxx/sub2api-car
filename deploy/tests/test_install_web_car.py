"""Offline bootstrap contracts; Docker and systemd are simulated boundaries."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import unittest
from unittest import mock

import test_upgrade_car as upgrade_fixtures
from test_upgrade_car import COMMIT, OLD_COMMIT, OLD_VERSION, VERSION, FakeDocker, contents, make_bundle

DEPLOY = Path(__file__).resolve().parents[1]
INSTALLER = DEPLOY / "install_car_web_update.py"
WEB_FILES = ("upgrade_car.py", "car_release.py", "car_update_service.py", "install_car_web_update.py",
             "sub2api-car-updater.service", "docker-compose.car-updater.yml")


def load_installer():
    spec = importlib.util.spec_from_file_location("car_bootstrap_test", INSTALLER)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def web_bundle(root, **kwargs):
    files = contents()
    for name in WEB_FILES:
        files["deploy/" + name] = (DEPLOY / name).read_bytes()
    files.update(kwargs.pop("files", {}))
    return make_bundle(root, files, **kwargs)


class HostCommands(FakeDocker):
    def __init__(self, install, config):
        super().__init__(install)
        self.config = config
        self.active = False
        self.enabled = False
        self.wired = False
        self.fail_service = False
        self.bad_config = False
        self.socket_inode = None
        self.socket_device = None
        self.loaded_unit = ""
        self.expected_unit = None
        self.drop_ins = ""
        self.kill_signal = "15"
        self.send_sighup = "no"
        self.exec_stop = ""
        self.exec_stop_post = ""
        self.runtime_directory = ""
        self.runtime_preserve = "no"
        self.enable_state_override = None
        self.enable_changes_state = True
        self.absent_enablement_empty = False

    def __call__(self, args, **kwargs):
        args = list(args)
        if args[0] == "systemctl":
            self.calls.append(args)
            code = 0
            output = b""
            if "is-active" in args:
                code = 0 if self.active else 3
            elif "is-enabled" in args:
                state = self.enable_state_override if self.enable_state_override is not None else ("enabled" if self.enabled else
                        "disabled" if self.expected_unit.exists() else "not-found")
                # systemctl returns success for static/indirect/runtime units;
                # that exit code alone does not prove persistent boot enablement.
                code = 0 if state in {"enabled", "enabled-runtime", "static", "indirect", "alias"} else 4 if state == "not-found" else 1
                output = b"" if "--quiet" in args else (state + "\n").encode()
                if state == "not-found" and self.absent_enablement_empty:
                    # systemd 249 reports an absent unit on stderr only.
                    code, output = 1, b""
            elif "stop" in args:
                self.active = False
            elif "start" in args:
                self.active = not self.fail_service
                code = 1 if self.fail_service else 0
            elif "enable" in args:
                if self.enable_changes_state:
                    self.enabled = True
            elif "daemon-reload" in args:
                self.loaded_unit = self.expected_unit
            elif "show" in args:
                properties = {"FragmentPath": str(self.loaded_unit), "DropInPaths": self.drop_ins,
                              "KillMode": "mixed", "TimeoutStopUSec": "infinity", "Restart": "always",
                              "KillSignal": self.kill_signal, "SendSIGHUP": self.send_sighup,
                              "ExecStop": self.exec_stop, "ExecStopPost": self.exec_stop_post,
                              "RuntimeDirectory": self.runtime_directory, "RuntimeDirectoryPreserve": self.runtime_preserve}
                requested = [arg.removeprefix("--property=") for arg in args if arg.startswith("--property=")]
                output = "".join(name + "=" + properties[name] + "\n" for name in requested).encode()
            return subprocess.CompletedProcess(args, code, output, b"PRIVATE_SYSTEMD_OUTPUT")
        if args[:2] == ["docker", "inspect"] and "{{json .}}" in args:
            self.calls.append(args)
            data = {"Config": {"Env": []}, "Mounts": []}
            if self.wired:
                data["Config"]["Env"] = ["UPDATE_MANAGED_SOCKET=/run/sub2api-car-updater/control.sock"]
                data["Mounts"] = [{"Type": "bind", "Source": str(self.config.socket.parent),
                                   "Destination": "/run/sub2api-car-updater", "RW": False}]
            return subprocess.CompletedProcess(args, 0, json.dumps(data).encode(), b"")
        if args[:2] == ["docker", "exec"] and args[3:] == ["stat", "-c", "%d:%i", "/run/sub2api-car-updater"]:
            self.calls.append(args)
            if self.socket_inode is None:
                return subprocess.CompletedProcess(args, 1, b"", b"missing mounted directory")
            output = "{}:{}\n".format(self.socket_device, self.socket_inode).encode()
            return subprocess.CompletedProcess(args, 0, output, b"")
        if "config" in args and "--format" in args:
            self.calls.append(args)
            data = {"services": {"sub2api": {"environment": {"UPDATE_MANAGED_SOCKET": "/run/sub2api-car-updater/control.sock"},
                     "volumes": [{"type": "bind", "source": str(self.config.socket.parent),
                                  "target": "/run/sub2api-car-updater", "read_only": not self.bad_config}]}}}
            return subprocess.CompletedProcess(args, 0, json.dumps(data).encode(), b"PRIVATE_ENV")
        if "up" in args:
            target = upgrade_fixtures.upgrader._env_version((self.install / "deploy/.env").read_bytes())[0]
            if not self.wired or "--force-recreate" in args or self.version != target:
                self.wired = True
                info = self.config.socket.parent.stat()
                self.socket_device, self.socket_inode = info.st_dev, info.st_ino
        return super().__call__(args, **kwargs)


class InstallerAvailabilityTests(unittest.TestCase):
    def test_bootstrap_and_templates_exist(self):
        for name in WEB_FILES:
            self.assertTrue((DEPLOY / name).is_file(), name + " must be bundled")


@unittest.skipUnless(INSTALLER.is_file(), "installer has not been implemented")
class InstallerTests(unittest.TestCase):
    def setUp(self):
        upgrade_fixtures.UpgradeTests.setUp(self)
        self.installer = load_installer()
        self.settings = self.installer.InstallConfig(self.install, self.root / "updates", self.root / "backups",
                       self.root / "state", self.root / "run/car-updater/control.sock", os.getuid(), os.getgid())
        self.system_root = self.root / "system"
        (self.system_root / "etc/systemd/system").mkdir(parents=True)
        self.unit = self.system_root / "etc/systemd/system/sub2api-car-updater.service"
        self.overlay = self.install / "deploy/docker-compose.car-updater.yml"
        self.archive = web_bundle(self.root)
        self.docker = HostCommands(self.install, self.settings)
        self.docker.expected_unit = self.unit
        self.status = {"protocol_version": 1, "phase": "idle"}

    def install_web(self):
        return self.installer.install(self.archive, self.settings, runner=self.docker,
                   system_root=self.system_root, report=self.reports.append, status_probe=lambda path: self.status)

    def test_standalone_bootstrap_uses_archive_helper_without_adjacent_modules(self):
        isolated = self.root / "standalone"
        isolated.mkdir()
        script = isolated / "install_car_web_update.py"
        shutil.copyfile(INSTALLER, script)
        code = ("import importlib.util; from pathlib import Path; "
                "s=importlib.util.spec_from_file_location('bootstrap', 'install_car_web_update.py'); "
                "m=importlib.util.module_from_spec(s); s.loader.exec_module(m); "
                "ctx=m.verified_bundle(Path(" + repr(str(self.archive)) + ")); "
                "h, release, b=ctx.__enter__(); "
                "assert b.version==" + repr(VERSION) + "; "
                "assert (release/'deploy/car_update_service.py').is_file(); ctx.__exit__(None,None,None)")
        result = subprocess.run([sys.executable, "-B", "-I", "-c", code], cwd=isolated,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(result.returncode, 0, result.stderr.decode())

    def test_first_install_preserves_operator_files_and_recreates_only_app_once(self):
        result = self.install_web()
        self.assertFalse(result.no_op)
        self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env.replace(OLD_VERSION.encode(), VERSION.encode()))
        self.assertEqual((self.install / "deploy/docker-compose.car.yml").read_bytes(), b"operator compose\n")
        self.assertEqual((self.install / "deploy/Caddyfile.car").read_bytes(), b"operator caddy\n")
        for directory in ("app", "postgres", "redis", "caddy-data", "caddy-config"):
            self.assertEqual((self.install / "deploy/car-data" / directory / "keep").read_bytes(), directory.encode())
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)
        for call in self.docker.calls:
            if "up" in call:
                self.assertIn(str(self.overlay), call)
                self.assertEqual(call[-1], "sub2api")
                self.assertIn("--no-deps", call)
        self.assertTrue(self.docker.enabled and self.docker.active)
        self.assertTrue((result.backup_dir / "database.dump").exists())
        self.assertEqual(self.docker.socket_inode, self.settings.socket.parent.stat().st_ino)
        self.assertEqual(stat.S_IMODE(self.settings.socket.parent.stat().st_mode), 0o750)
        self.assertNotIn("PRIVATE_", " ".join(self.reports))

    def test_checksum_archive_and_missing_web_files_fail_before_commands_or_settings(self):
        cases = ("checksum", "traversal", "link", "duplicate", "missing", "manifest")
        for case in cases:
            with self.subTest(case=case):
                self.archive = web_bundle(self.root)
                if case == "checksum":
                    Path(str(self.archive) + ".sha256").write_text("0" * 64 + "  release.tar.gz\n")
                elif case in {"traversal", "link", "duplicate"}:
                    item = tarfile.TarInfo("sub2api-car/../bad" if case == "traversal" else "sub2api-car/deploy/upgrade_car.py")
                    if case == "link":
                        item.type, item.linkname = tarfile.SYMTYPE, "/etc/passwd"
                    self.archive = web_bundle(self.root, extra=(item, None if case == "link" else b""))
                elif case == "missing":
                    self.archive = make_bundle(self.root, {**contents(), "deploy/upgrade_car.py": (DEPLOY / "upgrade_car.py").read_bytes()})
                else:
                    self.archive = web_bundle(self.root, omitted=("sub2api",))
                with self.assertRaises(self.installer.InstallError):
                    self.install_web()
                self.assertEqual(self.docker.calls, [])
                self.assertFalse(self.overlay.exists())
                self.assertFalse(self.unit.exists())

    def test_standalone_bounds_are_enforced_before_loading_helper(self):
        for limit in ("MAX_ARCHIVE_BYTES", "MAX_EXPANDED_BYTES", "MAX_MEMBERS", "MAX_HELPER_BYTES"):
            with self.subTest(limit=limit), mock.patch.object(self.installer, limit, 1):
                with self.assertRaises(self.installer.InstallError):
                    self.install_web()
        self.archive = web_bundle(self.root, pax_headers={"comment": "x" * 256})
        with mock.patch.object(self.installer, "MAX_TAR_METADATA_BYTES", 64):
            with self.assertRaises(self.installer.InstallError):
                self.install_web()
        self.assertEqual(self.docker.calls, [])

    def test_repeat_install_preserves_unit_overlay_and_socket_inode_without_app_restart(self):
        self.install_web()
        inode = self.settings.socket.parent.stat().st_ino
        self.unit.write_bytes(self.unit.read_bytes() + b"\n# operator comment\n")
        self.overlay.write_bytes(self.overlay.read_bytes() + b"\n# operator comment\n")
        before = (self.unit.read_bytes(), self.overlay.read_bytes())
        self.docker.calls.clear()
        result = self.install_web()
        self.assertTrue(result.no_op)
        self.assertFalse(any("up" in c or "build" in c for c in self.docker.calls))
        self.assertEqual(before, (self.unit.read_bytes(), self.overlay.read_bytes()))
        self.assertEqual(inode, self.settings.socket.parent.stat().st_ino)

    def test_manual_same_version_install_repairs_missing_mount_with_backup(self):
        self.docker.version, self.docker.commit = VERSION, COMMIT
        (self.install / "deploy/.env").write_bytes(self.env.replace(OLD_VERSION.encode(), VERSION.encode()))
        (self.install / "BUILD_VERSION").write_text(VERSION + "\n")
        (self.install / "BUILD_COMMIT").write_text(COMMIT + "\n")
        result = self.install_web()
        self.assertFalse(result.no_op)
        self.assertIsNotNone(result.backup_dir)
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 1)
        self.assertFalse(any("build" in c for c in self.docker.calls))

    def test_same_version_stale_bind_directory_is_backed_up_and_force_recreated(self):
        self.install_web()
        previous = self.settings.socket.parent.stat()
        self.settings.socket.parent.rename(self.settings.socket.parent.with_name("old-socket-directory"))
        self.settings.socket.parent.mkdir(mode=0o750)
        current = self.settings.socket.parent.stat()
        self.assertNotEqual(previous.st_ino, current.st_ino)
        self.docker.calls.clear()
        result = self.install_web()
        self.assertFalse(result.no_op)
        self.assertIsNotNone(result.backup_dir)
        self.assertTrue((result.backup_dir / "database.dump").exists())
        up = [call for call in self.docker.calls if "up" in call]
        self.assertEqual(len(up), 1)
        self.assertIn("--force-recreate", up[0])
        self.assertIn("--no-deps", up[0])
        self.assertEqual(up[0][-1], "sub2api")
        self.assertEqual((self.docker.socket_device, self.docker.socket_inode), (current.st_dev, current.st_ino))
        self.assertFalse(any("build" in c for c in self.docker.calls))

    def test_final_verification_rejects_stale_mount_even_when_host_protocol_is_healthy(self):
        self.install_web()
        self.reports.clear()
        def healthy_host_with_stale_container(path):
            self.assertEqual(path, self.settings.socket)
            self.docker.socket_inode += 1
            return {"protocol_version": 1, "phase": "idle"}
        with self.assertRaises(self.installer.InstallError):
            self.installer.install(self.archive, self.settings, runner=self.docker, system_root=self.system_root,
                                   report=self.reports.append, status_probe=healthy_host_with_stale_container)
        self.assertFalse(any("Web updater installation complete" in line for line in self.reports))

    def test_daemon_start_or_protocol_failure_never_claims_complete_and_retry_converges(self):
        self.docker.fail_service = True
        with self.assertRaisesRegex(self.installer.InstallError, "backup|recovery|retry"):
            self.install_web()
        self.assertEqual(self.docker.version, VERSION)
        self.assertFalse(any("Web updater installation complete" in line for line in self.reports))
        self.assertTrue(self.overlay.is_file() and self.unit.is_file())
        self.docker.fail_service = False
        self.status = {"protocol_version": 2}
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.status = {"protocol_version": 1, "phase": "idle"}
        self.docker.calls.clear()
        self.assertTrue(self.install_web().no_op)
        self.assertFalse(any("up" in c for c in self.docker.calls))

    def test_failure_before_switch_restores_settings_and_keeps_old_app(self):
        self.docker.fail = "build"
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertFalse(self.overlay.exists())
        self.assertFalse(self.unit.exists())
        self.assertEqual(self.docker.version, OLD_VERSION)
        self.assertFalse(any("up" in c for c in self.docker.calls))
        self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
        self.assertTrue(list(self.settings.state_dir.glob("bootstrap-*/settings.json")))

    def test_invalid_paths_permissions_and_unrelated_unit_fail_without_mutation(self):
        self.unit.write_text("[Service]\nExecStart=/bin/false\n")
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertEqual(self.unit.read_text(), "[Service]\nExecStart=/bin/false\n")
        self.assertFalse(self.overlay.exists())
        self.assertFalse(any("stop" in c or "up" in c for c in self.docker.calls))
        self.unit.unlink()
        self.settings.state_dir = self.install / "state"
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.settings.state_dir = self.root / "state with spaces"
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.settings.state_dir = self.root / "state"
        self.overlay.symlink_to(self.install / "deploy/docker-compose.car.yml")
        with self.assertRaises(self.installer.InstallError):
            self.install_web()

    def test_service_unit_drains_workers_and_never_removes_socket_directory(self):
        self.install_web()
        unit = self.unit.read_text()
        for setting in ("Restart=always", "KillMode=mixed", "KillSignal=SIGTERM", "SendSIGHUP=no",
                        "TimeoutStopSec=infinity", "UMask=0077", "User=root"):
            self.assertIn(setting, unit)
        self.assertNotIn("RuntimeDirectory=", unit)
        self.assertNotIn("ExecStopPost=", unit)
        for flag in ("--install-dir", "--updates-dir", "--backups-dir", "--state-dir", "--socket", "--app-uid", "--app-gid"):
            self.assertIn(flag, unit)

    def test_compose_mount_validation_failure_precedes_live_settings(self):
        self.docker.bad_config = True
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertFalse(self.overlay.exists())
        self.assertFalse(self.unit.exists())
        self.assertFalse(self.settings.socket.parent.exists())
        self.assertFalse(any("stop" in c or "up" in c for c in self.docker.calls))

    def test_bootstrap_refuses_downgrade_and_commit_conflict_before_settings(self):
        for version, commit in (("0.1.0-car.99", COMMIT), (VERSION, OLD_COMMIT)):
            with self.subTest(version=version):
                self.docker.version, self.docker.commit = version, commit
                (self.install / "deploy/.env").write_bytes(self.env.replace(OLD_VERSION.encode(), version.encode()))
                with self.assertRaises(self.installer.InstallError):
                    self.install_web()
                self.assertFalse(self.overlay.exists())
                self.assertFalse(any("up" in c or "build" in c for c in self.docker.calls))

    def test_cli_requires_root_linux_amd64(self):
        for uid, platform, machine in ((501, "linux", "x86_64"), (0, "darwin", "x86_64"), (0, "linux", "arm64")):
            with mock.patch.object(self.installer.os, "geteuid", return_value=uid), \
                 mock.patch.object(self.installer.sys, "platform", platform), \
                 mock.patch.object(self.installer.platform, "machine", return_value=machine), \
                 contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(self.installer.main([str(self.archive)]), 1)

    def test_manual_recovery_unknown_and_active_status_cannot_claim_usable_install(self):
        self.install_web()
        for phase in ("manual_recovery_required", "unknown", "preparing", "applying", None, []):
            with self.subTest(phase=phase):
                self.status = {"protocol_version": 1, "phase": phase}
                self.reports.clear()
                with self.assertRaises(self.installer.InstallError):
                    self.install_web()
                self.assertFalse(any("Web updater installation complete" in s for s in self.reports))
        for phase in ("idle", "ready", "succeeded", "failed", "rolled_back"):
            with self.subTest(phase=phase):
                self.status = {"protocol_version": 1, "phase": phase}
                self.assertTrue(self.install_web().no_op)

    def test_unsafe_systemd_dropin_is_rejected_before_stopping_daemon(self):
        self.install_web()
        self.docker.calls.clear()
        self.docker.drop_ins = "/etc/systemd/system/sub2api-car-updater.service.d/override.conf"
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertFalse(any("stop" in c or "up" in c for c in self.docker.calls))
        self.assertTrue(self.docker.active)

    def _assert_existing_unit_rejected_before_mutations(self, expected_unit):
        previous_env = (self.install / "deploy/.env").read_bytes()
        previous_overlay = self.overlay.read_bytes()
        previous_records = set(self.settings.state_dir.glob("bootstrap-*"))
        previous_backups = set(self.settings.backups_dir.iterdir())
        previous_inode = self.settings.socket.parent.stat().st_ino
        self.docker.calls.clear()
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertEqual(self.unit.read_bytes(), expected_unit)
        self.assertEqual((self.install / "deploy/.env").read_bytes(), previous_env)
        self.assertEqual(self.overlay.read_bytes(), previous_overlay)
        self.assertEqual(set(self.settings.state_dir.glob("bootstrap-*")), previous_records)
        self.assertEqual(set(self.settings.backups_dir.iterdir()), previous_backups)
        self.assertEqual(self.settings.socket.parent.stat().st_ino, previous_inode)
        self.assertFalse(any("stop" in c or "up" in c or "enable" in c or "daemon-reload" in c
                             for c in self.docker.calls))

    def test_raw_unsafe_stop_signals_or_hangup_are_rejected_before_stop(self):
        self.install_web()
        original = self.unit.read_text()
        for key, value, separator in (("KillSignal", "SIGKILL", "="), ("KillSignal", "SIGQUIT", "="),
                                      ("SendSIGHUP", "yes", "="), ("KillSignal", "SIGKILL", " = "),
                                      ("SendSIGHUP", "yes", " = ")):
            with self.subTest(key=key, value=value, separator=separator):
                text = "\n".join(line for line in original.splitlines() if line.partition("=")[0].strip() != key) + "\n"
                text = text.replace("KillMode=mixed", "KillMode=mixed\n" + key + separator + value)
                self.unit.write_text(text)
                self._assert_existing_unit_rejected_before_mutations(text.encode())

    def test_unsafe_loaded_signals_are_rejected_even_when_disk_unit_is_safe(self):
        self.install_web()
        safe_unit = self.unit.read_bytes()
        for signal_number, hangup in (("9", "no"), ("3", "no"), ("15", "yes")):
            with self.subTest(signal=signal_number, hangup=hangup):
                # Disk was corrected without daemon-reload; stop still uses
                # the old loaded signal settings and must not be issued.
                self.docker.kill_signal, self.docker.send_sighup = signal_number, hangup
                self._assert_existing_unit_rejected_before_mutations(safe_unit)

    def test_unsafe_loaded_stop_hooks_or_directory_cleanup_are_rejected_when_disk_is_safe(self):
        self.install_web()
        safe_unit = self.unit.read_bytes()
        for stop, post, directory, preserve in (("{ path=/bin/false ; argv[]=/bin/false ; }", "", "", "no"),
                                                ("", "{ path=/bin/rm ; argv[]=/bin/rm -rf /run/sub2api-car-updater ; }", "", "no"),
                                                ("", "", "sub2api-car-updater", "no"),
                                                ("", "", "sub2api-car-updater", "restart")):
            with self.subTest(stop=stop, post=post, directory=directory, preserve=preserve):
                self.docker.exec_stop, self.docker.exec_stop_post = stop, post
                self.docker.runtime_directory, self.docker.runtime_preserve = directory, preserve
                self._assert_existing_unit_rejected_before_mutations(safe_unit)

    def test_static_unit_without_install_section_is_rejected_before_stop(self):
        self.install_web()
        static = self.unit.read_text().split("[Install]", 1)[0]
        self.unit.write_text(static)
        self.docker.enabled = False
        self.docker.enable_state_override = "static"
        self.docker.enable_changes_state = False
        self._assert_existing_unit_rejected_before_mutations(static.encode())

    def test_unit_must_install_into_server_boot_target(self):
        self.install_web()
        original = self.unit.read_text()
        for dependency in ("", "WantedBy=graphical.target", "WantedBy=custom.target", "WantedBy=multi-user.target\nWantedBy="):
            with self.subTest(dependency=dependency):
                text = original.split("[Install]", 1)[0] + "[Install]\n" + dependency + "\n"
                self.unit.write_text(text)
                self._assert_existing_unit_rejected_before_mutations(text.encode())

    def test_enable_success_requires_explicit_persistent_enabled_state(self):
        self.install_web()
        self.docker.enable_changes_state = False
        self.docker.enabled = False
        for state in ("static", "enabled-runtime", "indirect", "disabled", "alias"):
            with self.subTest(state=state):
                self.docker.enable_state_override = state
                self.docker.calls.clear()
                self.reports.clear()
                with self.assertRaises(self.installer.InstallError):
                    self.install_web()
                self.assertFalse(any("Web updater installation complete" in line for line in self.reports))
                self.assertTrue(any("enable" in call for call in self.docker.calls))
                self.assertFalse(self.docker.enabled)

    def test_first_install_allows_legacy_absent_unit_enablement_with_empty_stdout(self):
        self.docker.absent_enablement_empty = True
        self.assertFalse(self.install_web().no_op)
        self.assertTrue(self.docker.active and self.docker.enabled)

    def test_existing_unit_with_unknown_empty_enablement_fails_before_stop(self):
        self.install_web()
        self.docker.enable_state_override = ""
        self._assert_existing_unit_rejected_before_mutations(self.unit.read_bytes())

    def test_empty_enablement_is_never_accepted_after_enable(self):
        self.docker.enable_state_override = ""
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertTrue(any("enable" in call for call in self.docker.calls))
        self.assertFalse(any("Web updater installation complete" in line for line in self.reports))

    def test_safe_default_signals_and_required_boot_dependency_preserve_operator_bytes(self):
        self.install_web()
        text = "\n".join(line for line in self.unit.read_text().splitlines()
                         if not line.startswith(("KillSignal=", "SendSIGHUP="))) + "\n"
        text = text.replace("WantedBy=multi-user.target", "RequiredBy=multi-user.target\nWantedBy=graphical.target")
        self.unit.write_text(text)
        self.docker.calls.clear()
        self.assertTrue(self.install_web().no_op)
        self.assertEqual(self.unit.read_text(), text)
        self.assertFalse(any("up" in call for call in self.docker.calls))

    def test_daemon_supported_sigint_is_a_safe_explicit_stop_signal(self):
        self.install_web()
        text = "\n".join(line for line in self.unit.read_text().splitlines() if not line.startswith("KillSignal=")) + "\n"
        text = text.replace("KillMode=mixed", "KillMode=mixed\nKillSignal=SIGINT")
        self.unit.write_text(text)
        self.docker.kill_signal = "2"
        self.assertTrue(self.install_web().no_op)
        self.assertEqual(self.unit.read_text(), text)

    def test_safe_unit_whitespace_and_preserved_runtime_directory_keep_operator_bytes(self):
        self.install_web()
        text = self.unit.read_text().replace("KillMode=mixed", "KillMode = mixed\nRuntimeDirectory = sub2api-car-updater\nRuntimeDirectoryPreserve = yes")
        text = text.replace("KillSignal=SIGTERM", "KillSignal = SIGTERM").replace("SendSIGHUP=no", "SendSIGHUP = no")
        text = text.replace("WantedBy=multi-user.target", "WantedBy = multi-user.target")
        self.unit.write_text(text)
        self.docker.runtime_directory, self.docker.runtime_preserve = "sub2api-car-updater", "yes"
        self.assertTrue(self.install_web().no_op)
        self.assertEqual(self.unit.read_text(), text)

    def _assert_definition_rejected_without_mutations(self):
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertFalse(self.unit.exists())
        self.assertFalse(self.overlay.exists())
        self.assertFalse(self.settings.socket.parent.exists())
        self.assertFalse(self.settings.state_dir.exists())
        self.assertFalse(self.settings.backups_dir.exists())
        self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
        self.assertEqual(self.docker.version, OLD_VERSION)
        self.assertFalse(any("up" in c or "build" in c or "stop" in c or "daemon-reload" in c
                             for c in self.docker.calls))

    def test_first_install_reported_dropin_fails_before_any_live_mutation(self):
        self.docker.drop_ins = "/etc/systemd/system/sub2api-car-updater.service.d/override.conf"
        self._assert_definition_rejected_without_mutations()

    def test_first_install_inactive_vendor_service_fails_before_any_live_mutation(self):
        self.docker.loaded_unit = "/usr/lib/systemd/system/sub2api-car-updater.service"
        self._assert_definition_rejected_without_mutations()

    def test_first_install_latent_dropin_is_rejected_even_when_not_reported(self):
        directory = self.unit.parent / (self.installer.SERVICE + ".d")
        directory.mkdir()
        (directory / "override.conf").write_text("[Service]\nRestart=no\n")
        self._assert_definition_rejected_without_mutations()

    def test_first_install_prefix_dropin_is_rejected_even_when_not_reported(self):
        directory = self.system_root / "usr/lib/systemd/system/sub2api-car-.service.d"
        directory.mkdir(parents=True)
        (directory / "override.conf").write_text("[Service]\nRestart=no\n")
        self._assert_definition_rejected_without_mutations()

    def test_unit_cleanup_hook_cannot_remove_a_live_socket_mount(self):
        self.install_web()
        self.unit.write_text(self.unit.read_text().replace("KillMode=mixed", "KillMode=mixed\nExecStop=/bin/rm -rf /run/sub2api-car-updater"))
        self.docker.calls.clear()
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertFalse(any("stop" in c for c in self.docker.calls))

    def test_shared_system_directories_cannot_be_used_for_private_storage_or_socket(self):
        self.settings.socket = Path("/run/control.sock")
        with self.assertRaisesRegex(self.installer.InstallError, "dedicated"):
            self.install_web()
        self.assertEqual(self.docker.calls, [])
        self.settings.socket = self.root / "run/car-updater/control.sock"
        self.settings.state_dir = Path("/var/lib")
        with self.assertRaisesRegex(self.installer.InstallError, "dedicated"):
            self.install_web()
        self.assertEqual(self.docker.calls, [])

    def test_existing_world_writable_settings_are_rejected_before_commands(self):
        self.overlay.write_bytes(b"operator config\n")
        self.overlay.chmod(0o666)
        with self.assertRaises(self.installer.InstallError):
            self.install_web()
        self.assertEqual(self.docker.calls, [])

    def test_status_probe_sends_direct_unix_get_and_reads_protocol_response(self):
        path = self.root / "probe.sock"
        received = []
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(server.close)
        server.bind(str(path))
        server.listen(1)
        server.settimeout(3)
        def serve():
            connection, unused_address = server.accept()
            with connection:
                received.append(connection.recv(4096))
                data = b'{"protocol_version":1,"phase":"idle"}'
                connection.sendall(b"HTTP/1.0 200 OK\r\nContent-Length: " + str(len(data)).encode() + b"\r\n\r\n" + data)
        worker = threading.Thread(target=serve)
        worker.start()
        try:
            self.assertEqual(self.installer._status_probe(path), {"protocol_version": 1, "phase": "idle"})
        finally:
            worker.join(5)
        self.assertFalse(worker.is_alive())
        self.assertTrue(received[0].startswith(b"GET /v1/status HTTP/1.0\r\n"))

    def test_systemctl_query_errors_are_bounded_and_do_not_expose_child_output(self):
        original = self.docker.__call__
        def failing_query(args, **kwargs):
            if args[0] == "systemctl":
                raise subprocess.TimeoutExpired(args, 30, output=b"PRIVATE_OUTPUT", stderr=b"PRIVATE_ERROR")
            return original(args, **kwargs)
        with self.assertRaises(self.installer.InstallError) as error:
            self.installer.install(self.archive, self.settings, runner=failing_query, system_root=self.system_root,
                                   report=self.reports.append, status_probe=lambda path: self.status)
        self.assertNotIn("PRIVATE", str(error.exception))
        self.assertFalse(self.overlay.exists())

    def test_rollback_preserves_old_app_and_prebootstrap_settings(self):
        self.docker.fail_start = True
        with self.assertRaisesRegex(self.installer.InstallError, "rolled back"):
            self.install_web()
        self.assertEqual(self.docker.version, OLD_VERSION)
        self.assertEqual((self.install / "deploy/.env").read_bytes(), self.env)
        self.assertFalse(self.overlay.exists())
        self.assertFalse(self.unit.exists())
        self.assertEqual(len([c for c in self.docker.calls if "up" in c]), 2)
        self.assertFalse(any("pg_restore" in c and "--list" not in c for c in self.docker.calls))

    def test_pending_migration_failure_retains_recovery_files_without_database_restore(self):
        self.docker.ledger = {}
        self.docker.fail_start = True
        with self.assertRaisesRegex(self.installer.InstallError, "manual recovery"):
            self.install_web()
        self.assertTrue(self.overlay.exists())
        self.assertFalse(self.docker.active)
        records = list(self.settings.backups_dir.glob("*/recovery.json"))
        self.assertEqual(json.loads(records[0].read_text())["status"], "manual_recovery_required")
        self.assertFalse(any("pg_restore" in c and "--list" not in c for c in self.docker.calls))

    def test_same_version_with_missing_host_module_is_repaired(self):
        self.install_web()
        (self.install / "deploy/car_update_service.py").unlink()
        self.docker.calls.clear()
        result = self.install_web()
        self.assertFalse(result.no_op)
        self.assertTrue((self.install / "deploy/car_update_service.py").is_file())
        self.assertTrue((result.backup_dir / "database.dump").exists())

    def test_lock_prevents_manual_upgrade_during_bootstrap_mutations(self):
        attempted = []
        original = self.docker.__call__
        def runner(args, **kwargs):
            if "build" in args:
                with self.assertRaisesRegex(upgrade_fixtures.upgrader.UpgradeError, "another upgrade"):
                    with upgrade_fixtures.upgrader.installation_lock(self.install):
                        self.fail("installer released its installation lock before the app transaction")
                attempted.append(True)
            return original(args, **kwargs)
        self.installer.install(self.archive, self.settings, runner=runner, system_root=self.system_root,
                               report=self.reports.append, status_probe=lambda path: self.status)
        self.assertEqual(attempted, [True])

    def test_failure_settings_restore_is_also_protected_by_installation_lock(self):
        self.docker.fail = "build"
        restore = self.installer._restore_setting
        checked = []
        def checked_restore(*args):
            with self.assertRaisesRegex(upgrade_fixtures.upgrader.UpgradeError, "another upgrade"):
                with upgrade_fixtures.upgrader.installation_lock(self.install):
                    self.fail("restoring settings must hold the installation lock")
            checked.append(True)
            return restore(*args)
        with mock.patch.object(self.installer, "_restore_setting", side_effect=checked_restore):
            with self.assertRaises(self.installer.InstallError):
                self.install_web()
        self.assertEqual(len(checked), 2)

    def test_daemon_is_drained_then_version_is_rechecked_before_live_changes(self):
        self.install_web()
        self.docker.calls.clear()
        original = self.docker.__call__
        def runner(args, **kwargs):
            result = original(args, **kwargs)
            if args[:2] == ["systemctl", "stop"]:
                self.docker.version = "0.1.0-car.99"
                (self.install / "deploy/.env").write_bytes(self.env.replace(OLD_VERSION.encode(), self.docker.version.encode()))
            return result
        with self.assertRaises(self.installer.InstallError):
            self.installer.install(self.archive, self.settings, runner=runner, system_root=self.system_root,
                                   report=self.reports.append, status_probe=lambda path: self.status)
        self.assertFalse(any("build" in c or "up" in c for c in self.docker.calls))

    def test_active_daemon_lock_blocks_settings_mutation(self):
        self.settings.state_dir.mkdir(mode=0o700)
        path = self.settings.state_dir / "daemon.lock"
        import fcntl
        with path.open("w") as handle:
            path.chmod(0o600)
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(self.installer.InstallError):
                self.install_web()
        self.assertFalse(self.overlay.exists())
        self.assertFalse(any("up" in c for c in self.docker.calls))

    def test_socket_source_may_be_created_by_docker_on_host_reboot(self):
        self.assertIn("create_host_path: true", (DEPLOY / "docker-compose.car-updater.yml").read_text())

    def test_cli_termination_uses_transaction_recovery_handler(self):
        observed = []
        previous = signal.getsignal(signal.SIGTERM)
        def interrupted(*args):
            handler = signal.getsignal(signal.SIGTERM)
            observed.append(callable(handler))
            if callable(handler):
                handler(signal.SIGTERM, None)
        with mock.patch.object(self.installer.os, "geteuid", return_value=0), \
             mock.patch.object(self.installer.sys, "platform", "linux"), \
             mock.patch.object(self.installer.platform, "machine", return_value="x86_64"), \
             mock.patch.object(self.installer, "install", side_effect=interrupted), \
             contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(self.installer.main([str(self.archive)]), 1)
        self.assertEqual(observed, [True])
        self.assertEqual(signal.getsignal(signal.SIGTERM), previous)


if __name__ == "__main__":
    unittest.main()
