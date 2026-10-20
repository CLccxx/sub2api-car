#!/usr/bin/env python3
"""Publish a verified complete carpool bundle, exposing it only after upload."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "deploy"))
import upgrade_car

REPOSITORY = "CLccxx/sub2api-car"


class PublishError(Exception):
    pass


def run(args, cwd=ROOT):
    try:
        return subprocess.run(args, cwd=cwd, check=True, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout
    except (OSError, subprocess.CalledProcessError):
        raise PublishError("Publication command failed. Any created release remains a draft until all assets are verified.") from None


def verify_remote_tag(tag, commit, runner):
    endpoint = "repos/" + REPOSITORY + "/git/ref/tags/" + tag
    for _ in range(5):
        try:
            target = json.loads(runner(["gh", "api", endpoint]))["object"]
            sha, kind = target["sha"], target["type"]
            if not isinstance(sha, str) or not upgrade_car.COMMIT_RE.fullmatch(sha):
                raise ValueError()
        except (ValueError, KeyError, TypeError):
            raise PublishError("The remote release tag could not be resolved safely.") from None
        if kind == "commit":
            if sha != commit:
                raise PublishError("The remote release tag moved away from the packaged source commit; refusing publication.")
            return
        if kind != "tag":
            break
        endpoint = "repos/" + REPOSITORY + "/git/tags/" + sha
    raise PublishError("The remote release tag is not a bounded commit reference.")


def publish(tag, commit, output, runner=run, verifier=upgrade_car.verify_archive):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+-car\.[0-9]+", tag) or not upgrade_car.COMMIT_RE.fullmatch(commit):
        raise PublishError("A custom release tag and full source commit are required.")
    version = tag[1:]
    name = "sub2api-car-" + version + "-linux-amd64"
    output = Path(output).resolve()
    archive = output / (name + ".tar.gz")
    checksum = output / (name + ".tar.gz.sha256")
    bootstrap = output / name / "deploy/install_car_web_update.py"
    for path in (archive, checksum, bootstrap):
        if path.is_symlink() or not path.is_file():
            raise PublishError("The complete archive, checksum and bootstrap are required.")
    bundle = verifier(archive)
    if bundle.version != version or bundle.commit != commit:
        raise PublishError("The deployment bundle does not match the release tag and source commit.")
    with tarfile.open(archive, "r:gz") as verified_archive:
        member = verified_archive.extractfile("sub2api-car/deploy/install_car_web_update.py")
        if member is None:
            raise PublishError("The verified bundle is missing its bootstrap.")
        with member:
            expected_bootstrap = member.read(1024 * 1024 + 1)
    if len(expected_bootstrap) > 1024 * 1024 or bootstrap.read_bytes() != expected_bootstrap:
        raise PublishError("The standalone bootstrap does not match the verified archive.")
    tag_commit = runner(["git", "rev-parse", "--verify", "refs/tags/" + tag + "^{commit}"]).strip()
    if tag_commit != commit:
        raise PublishError("The Git tag does not point to the packaged source commit.")
    verify_remote_tag(tag, commit, runner)
    files = (archive, checksum, bootstrap)
    notes = ("网页更新版本 " + version + "\n\n"
             "在管理面板的版本菜单刷新，点击更新完成准备，再点击立即重启。\n"
             "首次启用网页更新，请参阅仓库 deploy/CARPOOL_DEPLOYMENT.md。\n\n"
             "Source commit: " + commit + "\n")
    with tempfile.TemporaryDirectory(prefix="car-release-notes-") as temporary:
        notes_path = Path(temporary) / "notes.txt"
        notes_path.write_text(notes, encoding="utf-8")
        # create rejects any existing release. No --clobber or asset overwrite.
        runner(["gh", "release", "create", tag, "--repo", REPOSITORY, "--draft",
                "--verify-tag", "--target", commit, "--title", tag,
                "--notes-file", str(notes_path)])
        runner(["gh", "release", "upload", tag, *map(str, files), "--repo", REPOSITORY])
        raw = runner(["gh", "release", "view", tag, "--repo", REPOSITORY,
                      "--json", "tagName,isDraft,isPrerelease,targetCommitish,assets"])
        try:
            release = json.loads(raw)
            expected = {path.name: path.stat().st_size for path in files}
            assets = release["assets"]
            valid = (release["tagName"] == tag and release["isDraft"] is True
                     and release["isPrerelease"] is False
                     and len(assets) == len(expected)
                     and {asset["name"]: asset["size"] for asset in assets} == expected)
        except (ValueError, KeyError, TypeError):
            valid = False
        if not valid:
            raise PublishError("Uploaded release assets are incomplete or mismatched; the release remains a draft.")
        # GitHub ignores target_commitish when a tag exists; the peeled remote
        # tag, not that advisory metadata field, establishes source identity.
        verify_remote_tag(tag, commit, runner)
        runner(["gh", "release", "edit", tag, "--repo", REPOSITORY,
                "--draft=false", "--prerelease=false", "--latest", "--verify-tag"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("--commit", required=True)
    parser.add_argument("--output-dir", type=Path, default=ROOT / "output")
    args = parser.parse_args()
    try:
        publish(args.tag, args.commit, args.output_dir)
    except (PublishError, upgrade_car.UpgradeError, OSError, tarfile.TarError, KeyError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    print("Published complete custom release: " + args.tag)
    return 0


if __name__ == "__main__":
    sys.exit(main())
