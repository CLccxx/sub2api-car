"""Publication tests with a fake GitHub CLI; never create a real release."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest

SPEC = importlib.util.spec_from_file_location("publish_car", Path(__file__).resolve().parents[2] / ".github/release-tools/publish_car.py")
publisher = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(publisher)


class PublicationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.output = Path(temporary.name)
        self.version = "0.2.17-car.7"
        self.tag = "v" + self.version
        self.commit = "a" * 40
        self.name = "sub2api-car-" + self.version + "-linux-amd64"
        self.archive = self.output / (self.name + ".tar.gz")
        self.checksum = self.output / (self.name + ".tar.gz.sha256")
        self.bootstrap = self.output / self.name / "deploy/install_car_web_update.py"
        self.bootstrap.parent.mkdir(parents=True)
        self.bootstrap.write_bytes(b"# bootstrap fixture\n")
        with tarfile.open(self.archive, "w:gz") as archive:
            item = tarfile.TarInfo("sub2api-car/deploy/install_car_web_update.py")
            item.size = self.bootstrap.stat().st_size
            archive.addfile(item, io.BytesIO(self.bootstrap.read_bytes()))
        self.checksum.write_text(hashlib.sha256(self.archive.read_bytes()).hexdigest() + "  " + self.archive.name + "\n")
        self.calls = []
        self.tag_commit = self.commit
        self.remote_commits = [self.commit, self.commit]
        self.annotated = False
        self.metadata = {"tagName": self.tag, "isDraft": True, "isPrerelease": False, "targetCommitish": self.commit,
                         "assets": [{"name": p.name, "size": p.stat().st_size} for p in (self.archive, self.checksum, self.bootstrap)]}

    def runner(self, command):
        self.calls.append(command)
        if command[0] == "git":
            return self.tag_commit
        if command[:2] == ["gh", "api"]:
            if self.annotated and "/git/ref/tags/" in command[2]:
                return json.dumps({"object": {"type": "tag", "sha": "b" * 40}})
            return json.dumps({"object": {"type": "commit", "sha": self.remote_commits.pop(0)}})
        if command[1:3] == ["release", "view"]:
            return json.dumps(self.metadata)
        return ""

    def publish(self, runner=None):
        return publisher.publish(self.tag, self.commit, self.output, runner=runner or self.runner,
                                 verifier=lambda _: SimpleNamespace(version=self.version, commit=self.commit))

    def test_complete_verified_draft_is_published_as_full_latest(self):
        self.publish()
        commands = [cmd[2] for cmd in self.calls if cmd[:2] == ["gh", "release"]]
        self.assertEqual(commands, ["create", "upload", "view", "edit"])
        create = next(cmd for cmd in self.calls if cmd[1:3] == ["release", "create"])
        self.assertIn("--draft", create)
        self.assertIn("--verify-tag", create)
        self.assertIn("--draft=false", self.calls[-1])
        self.assertIn("--prerelease=false", self.calls[-1])
        self.assertIn("--latest", self.calls[-1])
        self.assertFalse(any("--clobber" in cmd for cmd in self.calls))

    def test_wrong_tag_commit_blocks_all_github_mutations(self):
        self.tag_commit = "b" * 40
        with self.assertRaises(publisher.PublishError):
            self.publish()
        self.assertFalse(any(cmd[0] == "gh" for cmd in self.calls))

    def test_upload_failure_leaves_draft(self):
        def fail(command):
            result = self.runner(command)
            if command[1:3] == ["release", "upload"]:
                raise publisher.PublishError("upload failed")
            return result
        with self.assertRaises(publisher.PublishError):
            self.publish(fail)
        self.assertFalse(any(cmd[1:3] == ["release", "edit"] for cmd in self.calls))

    def test_missing_or_wrong_remote_asset_never_publishes(self):
        for mode in ("missing", "wrong-size", "prerelease", "published"):
            with self.subTest(mode=mode):
                original = json.loads(json.dumps(self.metadata))
                self.calls.clear()
                self.remote_commits = [self.commit, self.commit]
                if mode == "missing": self.metadata["assets"].pop()
                if mode == "wrong-size": self.metadata["assets"][0]["size"] += 1
                if mode == "prerelease": self.metadata["isPrerelease"] = True
                if mode == "published": self.metadata["isDraft"] = False
                with self.assertRaises(publisher.PublishError):
                    self.publish()
                self.assertFalse(any(cmd[1:3] == ["release", "edit"] for cmd in self.calls))
                self.metadata = original

    def test_modified_bootstrap_never_uploads(self):
        self.bootstrap.write_bytes(b"changed")
        with self.assertRaises(publisher.PublishError):
            self.publish()
        self.assertEqual(self.calls, [])

    def test_invalid_tag_is_rejected_before_commands(self):
        self.tag = "v0.2.18"
        with self.assertRaises(publisher.PublishError):
            self.publish()
        self.assertEqual(self.calls, [])

    def test_missing_asset_does_not_create_release(self):
        self.checksum.unlink()
        with self.assertRaises(publisher.PublishError):
            self.publish()
        self.assertEqual(self.calls, [])

    def test_remote_tag_drift_blocks_draft_creation(self):
        self.remote_commits = ["c" * 40]
        with self.assertRaises(publisher.PublishError):
            self.publish()
        self.assertFalse(any(cmd[1:3] == ["release", "create"] for cmd in self.calls))

    def test_remote_tag_drift_after_upload_leaves_draft(self):
        self.remote_commits = [self.commit, "c" * 40]
        with self.assertRaises(publisher.PublishError):
            self.publish()
        self.assertTrue(any(cmd[1:3] == ["release", "upload"] for cmd in self.calls))
        self.assertFalse(any(cmd[1:3] == ["release", "edit"] for cmd in self.calls))

    def test_annotated_tags_are_resolved_before_both_mutation_stages(self):
        self.annotated = True
        self.publish()
        queries = [cmd for cmd in self.calls if cmd[:2] == ["gh", "api"]]
        self.assertEqual(len(queries), 4)
        self.assertTrue(queries[1][2].endswith("/git/tags/" + "b" * 40))

    def test_branch_commitish_metadata_does_not_override_verified_tag_identity(self):
        self.metadata["targetCommitish"] = "main"
        self.publish()
        self.assertEqual(self.calls[-1][1:3], ["release", "edit"])

    def test_malformed_remote_object_is_not_used_as_a_followup_url(self):
        def malformed(command):
            if command[:2] == ["gh", "api"]:
                self.calls.append(command)
                return json.dumps({"object": {"type": "tag", "sha": "../foreign"}})
            return self.runner(command)
        with self.assertRaises(publisher.PublishError):
            self.publish(malformed)
        self.assertEqual(len([cmd for cmd in self.calls if cmd[:2] == ["gh", "api"]]), 1)


if __name__ == "__main__":
    unittest.main()
