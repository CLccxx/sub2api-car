"""Release boundary tests; no Docker daemon or network is needed."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location(
    "package_car", Path(__file__).resolve().parents[1] / "package_car.py"
)
package = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = package
SPEC.loader.exec_module(package)


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "repository with spaces"
        self.root.mkdir()
        self.out = Path(self.temp.name) / "releases"
        files = {
            ".gitignore": "ignored-secret\n__pycache__/\nbackend/internal/web/dist/\n",
            "backend/go.mod": "module example.test/app\n\ngo 1.27.2\n",
            "backend/migrations/001_setup.sql": "\u2003\nCREATE TABLE example (id int);\n\u00a0",
            "backend/migrations/002_data.sql": "INSERT INTO example VALUES (1);\n",
            "backend/resources/model-pricing/pricing.json": "{}\n",
        }
        for name in package.RELEASE_FILES:
            files[name] = "# release fixture\n"
        for name, content in files.items():
            target = self.root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content, encoding="utf-8")
        self.git("init", "-q")
        self.git("config", "user.name", "Release Test")
        self.git("config", "user.email", "release@example.test")
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")
        self.commit = self.git("rev-parse", "HEAD").strip()
        self.calls = []

    def git(self, *args):
        return subprocess.check_output(
            ["git", *args], cwd=self.root, text=True, stderr=subprocess.STDOUT
        )

    def build(self, args, cwd, capture=False):
        self.calls.append((list(args), Path(cwd), capture))
        if "build" in args and "./cmd/server" in args:
            output_mount = next(item for item in args if item.endswith(":/output"))
            binary = Path(output_mount[:-len(":/output")]) / "sub2api"
            # ELF64, little endian, x86_64; the fake runner supplies version output.
            header = b"\x7fELF\x02\x01\x01" + b"\x00" * 11 + b"\x3e\x00"
            binary.write_bytes(header + b"fixture binary")
            binary.chmod(0o755)
        if "--version" in args and "/release/sub2api" in args:
            return "Sub2API 0.2.17-car.6 (commit: %s, built: fixture)\n" % self.commit
        return ""

    def release(self, **kwargs):
        return package.package_release(
            self.root, "0.2.17-car.6", output_dir=self.out,
            runner=self.build, report=lambda message: None, **kwargs
        )

    def test_build_order_metadata_and_complete_secret_free_archive(self):
        (self.root / "ignored-secret").write_text("must never enter the release")
        result = self.release()
        commands = [args for args, _, _ in self.calls]
        frontend = next(i for i, args in enumerate(commands) if args == ["pnpm", "run", "build"])
        backend = next(i for i, args in enumerate(commands) if "./cmd/server" in args)
        self.assertLess(frontend, backend)
        self.assertIn(["pnpm", "install", "--frozen-lockfile"], commands)
        self.assertIn("golang:1.27.2", commands[backend])
        self.assertIn("GOTOOLCHAIN=go1.27.2", commands[backend])
        self.assertIn("GOARCH=amd64", commands[backend])
        self.assertIn("main.Commit=" + self.commit, " ".join(commands[backend]))
        self.assertTrue(any("--check-only" in args for args in commands))
        with tarfile.open(result.archive, "r:gz") as archive:
            files = {item.name: archive.extractfile(item).read()
                     for item in archive.getmembers() if item.isfile()}
            self.assertTrue(all(item.isdir() or item.isfile() for item in archive.getmembers()))
        prefix = "sub2api-car/"
        self.assertNotIn(prefix + "ignored-secret", files)
        self.assertEqual(files[prefix + "BUILD_COMMIT"].decode().strip(), self.commit)
        catalog = json.loads(files[prefix + "MIGRATIONS.json"])
        expected = hashlib.sha256(b"CREATE TABLE example (id int);").hexdigest()
        self.assertEqual(catalog["files"]["001_setup.sql"], expected)
        manifest = files.pop(prefix + "SHA256SUMS").decode().splitlines()
        entries = dict(line.split("  ", 1)[::-1] for line in manifest)
        self.assertEqual(set(entries), {name[len(prefix):] for name in files})
        for name, digest in entries.items():
            self.assertEqual(digest, hashlib.sha256(files[prefix + name]).hexdigest())
        self.assertEqual(result.checksum.read_text(), result.sha256 + "  " + result.archive.name + "\n")
        self.assertEqual(hashlib.sha256(result.archive.read_bytes()).hexdigest(), result.sha256)

    def test_dirty_or_untracked_source_is_rejected_before_build(self):
        for name in ("Dockerfile.goreleaser", "unexpected.txt"):
            with self.subTest(name=name):
                path = self.root / name
                before = path.read_bytes() if path.exists() else None
                path.write_text("changed")
                with self.assertRaisesRegex(package.PackageError, "clean|干净|未提交"):
                    self.release()
                self.assertEqual(self.calls, [])
                if before is None:
                    path.unlink()
                else:
                    path.write_bytes(before)

    def test_invalid_version_is_rejected_before_commands(self):
        for version in ("latest", "0.2.17-car.6;touch /tmp/x", "../car.6", "-bad"):
            with self.subTest(version=version):
                with self.assertRaises(package.PackageError):
                    package.package_release(self.root, version, output_dir=self.out, runner=self.build)
        self.assertEqual(self.calls, [])

    def test_existing_release_is_not_overwritten(self):
        self.out.mkdir()
        old = self.out / "sub2api-car-0.2.17-car.6-linux-amd64.tar.gz"
        old.write_bytes(b"existing release")
        with self.assertRaises(package.PackageError):
            self.release()
        self.assertEqual(old.read_bytes(), b"existing release")
        self.assertEqual(self.calls, [])

    def test_build_failure_leaves_no_published_release(self):
        def fail(args, cwd, capture=False):
            if args == ["pnpm", "run", "build"]:
                raise package.PackageError("frontend build failed")
            return self.build(args, cwd, capture)
        with self.assertRaisesRegex(package.PackageError, "frontend"):
            package.package_release(self.root, "0.2.17-car.6", output_dir=self.out, runner=fail, report=lambda _: None)
        self.assertEqual(list(self.out.glob("*.tar.gz")), [])
        self.assertEqual(list(self.out.glob("*.sha256")), [])

    def test_wrong_compiled_version_is_not_published(self):
        def wrong(args, cwd, capture=False):
            result = self.build(args, cwd, capture)
            return result.replace("0.2.17-car.6", "0.2.17-car.5")
        with self.assertRaises(package.PackageError):
            package.package_release(self.root, "0.2.17-car.6", output_dir=self.out, runner=wrong, report=lambda _: None)
        self.assertEqual(list(self.out.glob("*.tar.gz")), [])

    def test_source_change_during_build_is_not_published(self):
        def changed(args, cwd, capture=False):
            result = self.build(args, cwd, capture)
            if "./cmd/server" in args:
                (self.root / "backend/go.mod").write_text("modified during build")
            return result
        with self.assertRaises(package.PackageError):
            package.package_release(self.root, "0.2.17-car.6", output_dir=self.out, runner=changed, report=lambda _: None)
        self.assertEqual(list(self.out.glob("*.tar.gz")), [])

    def test_tracked_resource_symlink_is_rejected(self):
        resource = self.root / "backend/resources/model-pricing/pricing.json"
        resource.unlink()
        resource.symlink_to(self.root / "backend/go.mod")
        self.git("add", ".")
        self.git("commit", "-qm", "unsafe link")
        with self.assertRaises(package.PackageError):
            self.release()
        self.assertEqual(self.calls, [])

    def test_go_trim_space_does_not_remove_ascii_record_separator(self):
        sql = self.root / "backend/migrations/002_data.sql"
        sql.write_bytes(b"\x1eSELECT 1;\x1e")
        self.git("add", ".")
        self.git("commit", "-qm", "checksum edge")
        self.assertEqual(package.migration_catalog(self.root)["files"][sql.name],
                         hashlib.sha256(sql.read_bytes()).hexdigest())

    def test_build_uses_commit_snapshot_without_ignored_local_dotenv(self):
        frontend = self.root / "frontend"
        frontend.mkdir()
        (frontend / "package.json").write_text("{}")
        with (self.root / ".gitignore").open("a") as handle:
            handle.write("frontend/.env.local\n")
        self.git("add", ".")
        self.git("commit", "-qm", "frontend fixture")
        self.commit = self.git("rev-parse", "HEAD").strip()
        private = frontend / ".env.local"
        private.write_text("VITE_API_BASE_URL=https://developer-api.invalid\n")
        def check_snapshot(args, cwd, capture=False):
            if args == ["pnpm", "run", "build"]:
                self.assertNotEqual(Path(cwd), frontend)
                self.assertTrue((Path(cwd) / "package.json").exists())
                self.assertFalse((Path(cwd) / ".env.local").exists())
            return self.build(args, cwd, capture)
        package.package_release(self.root, "0.2.17-car.6", output_dir=self.out, runner=check_snapshot, report=lambda _: None)
        self.assertTrue(private.exists())

    def test_command_does_not_inherit_frontend_build_overrides(self):
        def record(args, **kwargs):
            env = kwargs.get("env", os.environ)
            for key in ("VITE_API_BASE_URL", "NODE_ENV", "NODE_OPTIONS"):
                self.assertTrue(key not in env, "unexpected inherited build override: " + key)
            self.assertIn("PATH", env)
            return subprocess.CompletedProcess(args, 0, stdout="")
        with mock.patch.dict(os.environ, {"VITE_API_BASE_URL": "https://developer-api.invalid", "NODE_ENV": "development", "NODE_OPTIONS": "--require /tmp/local.js"}):
            with mock.patch.object(package.subprocess, "run", side_effect=record):
                package.run_command(["pnpm", "run", "build"], self.root)

    def test_metadata_and_toolchain_are_read_from_the_same_committed_snapshot(self):
        self.git("update-index", "--assume-unchanged", "backend/go.mod", "backend/migrations/001_setup.sql")
        (self.root / "backend/go.mod").write_text("module example.test/app\n\ngo 1.99.0\n")
        (self.root / "backend/migrations/001_setup.sql").write_text("uncommitted migration")
        self.assertEqual(self.git("status", "--porcelain"), "")
        result = self.release()
        build = next(args for args, _, _ in self.calls if "./cmd/server" in args)
        self.assertIn("GOTOOLCHAIN=go1.27.2", build)
        catalog = json.loads((result.directory / "MIGRATIONS.json").read_text())
        self.assertEqual(catalog["files"]["001_setup.sql"], hashlib.sha256(b"CREATE TABLE example (id int);").hexdigest())

    def test_interrupted_temporary_cleanup_does_not_leave_a_partial_release(self):
        real_cleanup = tempfile.TemporaryDirectory.cleanup
        def interrupted(instance):
            real_cleanup(instance)
            raise KeyboardInterrupt()
        with mock.patch.object(package.tempfile.TemporaryDirectory, "cleanup", interrupted):
            with self.assertRaises(KeyboardInterrupt):
                self.release()
        for suffix in ("", ".tar.gz", ".tar.gz.sha256"):
            self.assertFalse((self.out / ("sub2api-car-0.2.17-car.6-linux-amd64" + suffix)).exists())
        self.release()  # A retry of this unpublished version remains possible.


if __name__ == "__main__":
    unittest.main()
