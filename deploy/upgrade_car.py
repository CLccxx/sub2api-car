#!/usr/bin/env python3
"""Verify and upgrade a single-server Sub2API car installation (Python 3.9+).

Checksums establish integrity, not authorship: obtain the archive and expected
checksum through a trusted channel. Child process output is deliberately not
printed because Compose, the application, and database tools may expose secrets.
"""
import argparse
import contextlib
from dataclasses import dataclass
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
from typing import Dict, Optional, Tuple

MAX_ARCHIVE_BYTES = 1024 * 1024 * 1024
MAX_EXPANDED_BYTES = 2 * 1024 * 1024 * 1024
MAX_FILE_BYTES = 512 * 1024 * 1024
MAX_MEMBERS = 10000
MAX_METADATA_BYTES = 2 * 1024 * 1024
MAX_TAR_METADATA_BYTES = 16 * 1024
MAX_TOTAL_TAR_METADATA_BYTES = 16 * 1024 * 1024
CHUNK_BYTES = 1024 * 1024
VERSION_RE = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+-car\.[0-9]+\Z")
COMMIT_RE = re.compile(r"(?:[a-f0-9]{40}|[a-f0-9]{64})\Z")
HASH_RE = re.compile(r"[a-f0-9]{64}\Z")
SQL_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*\.sql\Z")
REQUIRED_FILES = frozenset({
    "sub2api", "BUILD_VERSION", "BUILD_COMMIT", "SHA256SUMS",
    "Dockerfile.goreleaser", ".dockerignore", "deploy/docker-entrypoint.sh",
    "deploy/docker-compose.car.yml", "deploy/Caddyfile.car", "deploy/init-car.sh",
    "deploy/CARPOOL_DEPLOYMENT.md",
})
OPTIONAL_FILES = frozenset({
    "deploy/upgrade_car.py", "MIGRATIONS.json", "deploy/car_release.py",
    "deploy/car_update_service.py", "deploy/install_car_web_update.py",
    "deploy/sub2api-car-updater.service", "deploy/docker-compose.car-updater.yml",
})
OPTIONAL_CONFIG = frozenset({"deploy/docker-compose.car-updater.yml"})
RETAINED_CONFIG = frozenset({"deploy/docker-compose.car.yml", "deploy/Caddyfile.car"}) | OPTIONAL_CONFIG


class UpgradeError(Exception):
    """A safe, user-visible failure message without captured command output."""


class _BoundedTarInfo(tarfile.TarInfo):
    def _proc_member(self, archive):
        # tarfile consumes PAX/GNU extension data before yielding a member. Bound
        # those allocations here, rather than only checking the yielded files.
        count = getattr(archive, "_car_header_count", 0) + 1
        archive._car_header_count = count
        if count > MAX_MEMBERS:
            raise UpgradeError("Archive contains too many headers.")
        extensions = (tarfile.XHDTYPE, tarfile.XGLTYPE, tarfile.GNUTYPE_LONGNAME, tarfile.GNUTYPE_LONGLINK)
        if self.type in extensions:
            total = getattr(archive, "_car_extension_bytes", 0) + self.size
            archive._car_extension_bytes = total
            if self.size < 0 or self.size > MAX_TAR_METADATA_BYTES or total > MAX_TOTAL_TAR_METADATA_BYTES:
                raise UpgradeError("Archive extension metadata is too large.")
        elif self.type not in (tarfile.REGTYPE, tarfile.AREGTYPE, tarfile.DIRTYPE):
            raise UpgradeError("Archive contains a link, sparse file, or special file.")
        elif self.size < 0 or self.size > MAX_FILE_BYTES or (self.isdir() and self.size != 0):
            raise UpgradeError("Archive member has an invalid size.")
        return super()._proc_member(archive)


@dataclass
class Bundle:
    archive: Path
    version: str
    commit: str
    files: Tuple[str, ...]
    migrations: Optional[Dict[str, str]]
    sha256: str


def _canonical_path(name):
    if (not isinstance(name, str) or not name or len(name) > 512
            or "\\" in name or any(ord(c) < 32 or ord(c) == 127 for c in name)
            or name.startswith("/") or str(PurePosixPath(name)) != name
            or any(part in ("", ".", "..") for part in name.split("/"))):
        raise UpgradeError("Archive contains an unsafe or noncanonical path.")
    if any(part.startswith(".env") or part == "car-data" for part in name.split("/")):
        raise UpgradeError("Archive contains a secret or persistent-data path.")
    return name


def _allowed_file(name):
    _canonical_path(name)
    return name in REQUIRED_FILES | OPTIONAL_FILES or name.startswith("backend/resources/")


def _hash_stream(stream, limit):
    digest = hashlib.sha256()
    length = 0
    while True:
        data = stream.read(CHUNK_BYTES)
        if not data:
            return digest.hexdigest()
        length += len(data)
        if length > limit:
            raise UpgradeError("Archive exceeds the permitted size.")
        digest.update(data)


def _open_regular(path):
    fd = os.open(str(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise UpgradeError("Expected a regular file; links and special files are not allowed.")
        return os.fdopen(fd, "rb")
    except BaseException:
        os.close(fd)
        raise


def _expected_checksum(archive, sha256):
    if sha256 is not None:
        if not isinstance(sha256, str) or not HASH_RE.fullmatch(sha256):
            raise UpgradeError("--sha256 must be exactly 64 lowercase hexadecimal characters.")
        return sha256
    try:
        with _open_regular(Path(str(archive) + ".sha256")) as handle:
            data = handle.read(4097)
        if len(data) > 4096:
            raise ValueError()
        match = re.fullmatch(rb"([a-f0-9]{64})  ([^\r\n]+)\n?", data)
        if not match or match[2].decode("utf-8") != archive.name:
            raise ValueError()
        return match[1].decode("ascii")
    except (OSError, UnicodeError, ValueError):
        raise UpgradeError("A valid ARCHIVE.sha256 sidecar naming this archive is required.") from None


def _json_no_duplicates(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise UpgradeError("Metadata contains duplicate keys.")
        result[name] = value
    return result


def _migration_catalog(data):
    try:
        catalog = json.loads(data, object_pairs_hook=_json_no_duplicates)
        if (not isinstance(catalog, dict) or set(catalog) != {"format_version", "files"}
                or type(catalog["format_version"]) is not int or catalog["format_version"] != 1
                or not isinstance(catalog["files"], dict) or not catalog["files"]
                or len(catalog["files"]) > MAX_MEMBERS):
            raise ValueError()
        for name, checksum in catalog["files"].items():
            if (not SQL_RE.fullmatch(name) or len(name) > 255
                    or not isinstance(checksum, str) or not HASH_RE.fullmatch(checksum)):
                raise ValueError()
        return catalog["files"]
    except (ValueError, TypeError, UnicodeError):
        raise UpgradeError("Invalid MIGRATIONS.json catalog.") from None


def verify_archive(archive, sha256=None):
    """Validate a bundle without Docker, root, extraction, or an installation."""
    archive = Path(archive)
    expected = _expected_checksum(archive, sha256)
    try:
        with _open_regular(archive) as handle:
            if _hash_stream(handle, MAX_ARCHIVE_BYTES) != expected:
                raise UpgradeError("Archive SHA256 does not match the expected checksum.")
            handle.seek(0)
            if handle.read(2) != b"\x1f\x8b":
                raise UpgradeError("Expected a gzip-compressed tar archive.")
            handle.seek(0)
            hashes, metadata, seen = {}, {}, set()
            total = 0
            with tarfile.open(fileobj=handle, mode="r|gz", tarinfo=_BoundedTarInfo) as tar:
                for number, item in enumerate(tar, 1):
                    if number > MAX_MEMBERS:
                        raise UpgradeError("Archive contains too many entries.")
                    name = item.name
                    if item.isdir() and name.endswith("/"):
                        name = name[:-1]
                    _canonical_path(name)
                    if name in seen:
                        raise UpgradeError("Archive contains duplicate paths.")
                    seen.add(name)
                    if name == "sub2api-car" and item.isdir():
                        continue
                    if not name.startswith("sub2api-car/"):
                        raise UpgradeError("Archive must have one sub2api-car/ root.")
                    relative = name[len("sub2api-car/"):]
                    if item.isdir():
                        if (relative not in {"backend", "backend/resources", "deploy"}
                                and not relative.startswith("backend/resources/")):
                            raise UpgradeError("Archive contains an unexpected directory.")
                        continue
                    if not item.isreg() or item.issparse() or item.linkname:
                        raise UpgradeError("Archive contains a link, sparse file, or special file.")
                    if not _allowed_file(relative):
                        raise UpgradeError("Archive contains an unexpected file.")
                    total += item.size
                    if item.size < 0 or item.size > MAX_FILE_BYTES or total > MAX_EXPANDED_BYTES:
                        raise UpgradeError("Archive exceeds the permitted expanded size.")
                    if relative in {"BUILD_VERSION", "BUILD_COMMIT", "SHA256SUMS", "MIGRATIONS.json"}:
                        if item.size > MAX_METADATA_BYTES:
                            raise UpgradeError("Archive metadata is too large.")
                        data = tar.extractfile(item).read()
                        metadata[relative] = data
                        hashes[relative] = hashlib.sha256(data).hexdigest()
                    else:
                        with tar.extractfile(item) as stream:
                            hashes[relative] = _hash_stream(stream, MAX_FILE_BYTES)
            for name in seen:
                if name.startswith("sub2api-car/"):
                    relative = PurePosixPath(name[len("sub2api-car/"):])
                    if any(str(parent) in hashes for parent in relative.parents):
                        raise UpgradeError("Archive contains a file/directory path conflict.")
            if not REQUIRED_FILES.issubset(hashes) or not any(n.startswith("backend/resources/") for n in hashes):
                raise UpgradeError("Archive is missing required bundle files.")
            listed = {}
            for line in metadata["SHA256SUMS"].decode("utf-8").splitlines():
                match = re.fullmatch(r"([a-f0-9]{64})  (.+)", line)
                if not match:
                    raise UpgradeError("Invalid SHA256SUMS entry.")
                name = _canonical_path(match[2])
                if name in listed or name == "SHA256SUMS":
                    raise UpgradeError("SHA256SUMS contains duplicate or self-referencing entries.")
                listed[name] = match[1]
            if listed != {name: digest for name, digest in hashes.items() if name != "SHA256SUMS"}:
                raise UpgradeError("SHA256SUMS does not exactly match all bundle files.")
            version = metadata["BUILD_VERSION"].decode("ascii").removesuffix("\n")
            commit = metadata["BUILD_COMMIT"].decode("ascii").removesuffix("\n")
            if not VERSION_RE.fullmatch(version) or len(version) > 128 or not COMMIT_RE.fullmatch(commit):
                raise UpgradeError("Invalid BUILD_VERSION or BUILD_COMMIT metadata.")
            migrations = _migration_catalog(metadata["MIGRATIONS.json"]) if "MIGRATIONS.json" in metadata else None
            return Bundle(archive, version, commit, tuple(sorted(hashes)), migrations, expected)
    except (OSError, tarfile.TarError, EOFError, UnicodeError, ValueError):
        raise UpgradeError("Unable to read a complete, valid deployment archive.") from None


@dataclass
class UpgradeConfig:
    install_dir: Path = Path("/opt/sub2api-car")
    updates_dir: Path = Path("/opt/sub2api-car-updates")
    backups_dir: Path = Path("/opt/sub2api-car-backups")


@dataclass(frozen=True)
class PreparedImage:
    """Internal handoff from a verified preparation; never a CLI bypass flag."""
    archive_sha256: str
    version: str
    commit: str
    image_id: str


@dataclass
class UpgradeResult:
    version: str
    commit: str
    backup_dir: Optional[Path]
    stage_dir: Path
    no_op: bool = False


class SubprocessRunner:
    """No shell, inherited Compose overrides, interactive input, or output logs."""
    def __call__(self, args, cwd=None, stdin=None, stdout=None, timeout=180):
        environment = {"PATH": os.environ.get("PATH", os.defpath), "LANG": "C.UTF-8"}
        if "HOME" in os.environ:
            environment["HOME"] = os.environ["HOME"]
        return subprocess.run(args, cwd=cwd, stdin=stdin if stdin is not None else subprocess.DEVNULL,
                              stdout=stdout if stdout is not None else subprocess.PIPE,
                              stderr=subprocess.PIPE, timeout=timeout, env=environment, check=False)


def _run(runner, args, label, **kwargs):
    try:
        result = runner([str(arg) for arg in args], **kwargs)
    except (OSError, subprocess.SubprocessError):
        raise UpgradeError(label + " failed; command output was withheld to protect secrets.") from None
    if result.returncode:
        raise UpgradeError(label + " failed (exit {}). Command output was withheld to protect secrets.".format(result.returncode))
    return result


def _safe_directory(path, create=False, mode=0o755):
    path = Path(os.path.abspath(path))
    for current in reversed([path] + list(path.parents)):
        try:
            info = current.lstat()
        except FileNotFoundError:
            if not create:
                raise UpgradeError("Required installation directory is missing.") from None
            current.mkdir(mode=mode)
            info = current.lstat()
        if not stat.S_ISDIR(info.st_mode):
            raise UpgradeError("Directory paths must not contain symlinks or special files.")
    return path


def _private_directory(path):
    path = _safe_directory(path, create=True, mode=0o700)
    fd = os.open(str(path), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) & 0o022:
            raise UpgradeError("Staging and backup directories must be owned by this user and not writable by others.")
        # Earlier manual deployment created these dedicated parents with 0755.
        # Tighten only safe, self-owned directories; every release stays private.
        os.fchmod(fd, 0o700)
    finally:
        os.close(fd)
    return path


def _target(root, name, create_parents=False):
    # Never derive live paths from an unchecked archive path or a backup symlink.
    _canonical_path(name)
    parent = _safe_directory(root)
    for part in PurePosixPath(name).parts[:-1]:
        parent = parent / part
        if create_parents:
            _safe_directory(parent, create=True)
        else:
            try:
                if not stat.S_ISDIR(parent.lstat().st_mode):
                    raise UpgradeError("A managed parent path is a link or special file.")
            except FileNotFoundError:
                # New resources may introduce directories. Preflight must not
                # create them in the live installation before backup succeeds.
                pass
    target = parent / PurePosixPath(name).name
    try:
        info = target.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise UpgradeError("A managed live path is a link or special file.")
    except FileNotFoundError:
        pass
    return target


def _fsync_directory(path):
    fd = os.open(str(path), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _atomic_file(target, source=None, data=None, mode=0o600, owner=None):
    _safe_directory(target.parent)
    if target.is_symlink():
        raise UpgradeError("Refusing to replace a symlink.")
    fd, temporary = tempfile.mkstemp(prefix=".car-upgrade-", dir=str(target.parent))
    try:
        with os.fdopen(fd, "wb") as output:
            if source is not None:
                with _open_regular(source) as incoming:
                    shutil.copyfileobj(incoming, output, CHUNK_BYTES)
            else:
                output.write(data)
            output.flush()
            if owner is not None and (os.fstat(output.fileno()).st_uid, os.fstat(output.fileno()).st_gid) != owner:
                os.fchown(output.fileno(), *owner)
            os.fchmod(output.fileno(), mode)
            os.fsync(output.fileno())
        os.replace(temporary, target)
        _fsync_directory(target.parent)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class _HeldInstallationLock:
    """An internal live lock handoff for bootstrap; never exposed as a CLI flag."""
    def __init__(self, directory, fd):
        self.directory, self.fd, self.active = directory, fd, True

    def validate(self, directory):
        if not self.active or self.directory != directory:
            raise UpgradeError("The installation lock handoff is no longer valid.")
        held, current = os.fstat(self.fd), (directory / ".upgrade-car.lock").lstat()
        if (held.st_dev, held.st_ino) != (current.st_dev, current.st_ino):
            raise UpgradeError("The installation lock changed during bootstrap.")


@contextlib.contextmanager
def installation_lock(install_dir):
    """One stable lock per installation, regardless of the staging directory."""
    install_dir = _safe_directory(install_dir)
    fd = None
    try:
        try:
            fd = os.open(str(install_dir / ".upgrade-car.lock"), os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1
                    or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600):
                raise UpgradeError("The installation lock must be a private, owned regular file.")
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise UpgradeError("There is another upgrade running for this installation.") from None
        except OSError:
            raise UpgradeError("Unable to acquire a safe installation lock.") from None
        guard = _HeldInstallationLock(install_dir, fd)
        try:
            yield guard
        finally:
            guard.active = False
    finally:
        if fd is not None:
            os.close(fd)


def _stage_archive(archive, sha256, updates_dir):
    archive = Path(archive)
    expected = _expected_checksum(archive, sha256)
    updates_dir = _private_directory(updates_dir)
    stage = Path(tempfile.mkdtemp(prefix="release-", dir=str(updates_dir)))
    private_archive = stage / "bundle.tar.gz"
    try:
        with _open_regular(archive) as incoming, private_archive.open("xb") as output:
            os.fchmod(output.fileno(), 0o600)
            total = 0
            while True:
                data = incoming.read(CHUNK_BYTES)
                if not data:
                    break
                total += len(data)
                if total > MAX_ARCHIVE_BYTES:
                    raise UpgradeError("Archive exceeds the permitted size.")
                output.write(data)
            output.flush()
            os.fsync(output.fileno())
        # Only the private copy is ever validated, extracted, or built.
        bundle = verify_archive(private_archive, expected)
        release = stage / "sub2api-car"
        release.mkdir(mode=0o700)
        with tarfile.open(private_archive, "r|gz", tarinfo=_BoundedTarInfo) as tar:
            for item in tar:
                if not item.isreg():
                    continue
                name = item.name[len("sub2api-car/"):]
                if name not in bundle.files:
                    raise UpgradeError("Private archive changed after validation.")
                target = _target(release, name, create_parents=True)
                with tar.extractfile(item) as incoming, target.open("xb") as output:
                    shutil.copyfileobj(incoming, output, CHUNK_BYTES)
                target.chmod(0o755 if name == "sub2api" or name.endswith(".sh") else 0o644)
        return stage, release, bundle
    except (OSError, tarfile.TarError, EOFError):
        raise UpgradeError("Unable to stage the private deployment archive.") from None


ENV_KEY_RE = re.compile(rb"^[ \t]*(?:export[ \t]+)?SUB2API_CAR_VERSION[ \t]*=")
ENV_LINE_RE = re.compile(rb"([ \t]*(?:export[ \t]+)?SUB2API_CAR_VERSION[ \t]*=[ \t]*)(['\"]?)([0-9]+\.[0-9]+\.[0-9]+-car\.[0-9]+)\2([ \t]*(?:#[^\r\n]*)?)(\r?\n|$)")


def _env_version(data, replacement=None):
    lines = data.splitlines(keepends=True)
    matches = [(i, ENV_LINE_RE.fullmatch(line)) for i, line in enumerate(lines) if ENV_KEY_RE.match(line)]
    if len(matches) != 1 or matches[0][1] is None or b"\x00" in data:
        raise UpgradeError("The persistent .env must contain exactly one valid SUB2API_CAR_VERSION assignment.")
    index, match = matches[0]
    version = match[3].decode("ascii")
    if replacement is not None:
        lines[index] = match[1] + match[2] + replacement.encode("ascii") + match[2] + match[4] + match[5]
    return version, b"".join(lines)


def _compose(config, *args, env_file=None):
    deploy = config.install_dir / "deploy"
    command = ["docker", "compose", "--project-directory", str(deploy), "--env-file",
               str(env_file or deploy / ".env"), "-f", str(deploy / "docker-compose.car.yml")]
    overlay = _target(config.install_dir, "deploy/docker-compose.car-updater.yml")
    if overlay.exists():
        command.extend(["-f", str(overlay)])
    return command + list(args)


def _version_output(result):
    output = (result.stdout or b"") + (result.stderr or b"")
    matches = re.findall(rb"Sub2API ([0-9]+\.[0-9]+\.[0-9]+-car\.[0-9]+) \(commit: ([a-f0-9]{40}|[a-f0-9]{64}), built: [^\r\n)]*\)", output)
    if len(matches) != 1:
        raise UpgradeError("Unable to verify the application's exact version and commit.")
    return tuple(part.decode("ascii") for part in matches[0])


def _running_app(config, runner):
    result = _run(runner, _compose(config, "ps", "-q", "sub2api"), "Find running app")
    container = result.stdout.decode("ascii").strip()
    if not re.fullmatch(r"[A-Za-z0-9_-]+", container):
        raise UpgradeError("Expected exactly one running sub2api container.")
    template = "{{.Image}}\t{{.Config.Image}}\t{{.State.Status}}\t{{if .State.Health}}{{.State.Health.Status}}{{end}}"
    inspected = _run(runner, ["docker", "inspect", "--format", template, container], "Inspect running app")
    fields = inspected.stdout.decode("ascii").strip().split("\t")
    if len(fields) != 4 or not re.fullmatch(r"sha256:[a-f0-9]{64}", fields[0]) or fields[2] != "running":
        raise UpgradeError("The current app must be running with an inspectable image and health check.")
    version, commit = _version_output(_run(runner, ["docker", "exec", container, "/app/sub2api", "--version"], "Verify running version"))
    if fields[1] != "sub2api-car:" + version:
        raise UpgradeError("Running image tag does not match the running app version.")
    return {"container": container, "image_id": fields[0], "image_tag": fields[1],
            "version": version, "commit": commit, "healthy": fields[3] == "healthy"}


def _migration_snapshot(config, runner):
    try:
        result = _run(runner, _compose(config, "exec", "-T", "postgres", "psql", "-X", "-A", "-t", "-F", "\t",
                                      "-v", "ON_ERROR_STOP=1", "-U", "sub2api", "-d", "sub2api", "-c",
                                      "SELECT filename,checksum FROM schema_migrations ORDER BY filename"), "Read migration ledger")
        if len(result.stdout) > MAX_METADATA_BYTES:
            return None
        rows = {}
        for line in result.stdout.decode("ascii").splitlines():
            name, checksum = line.split("\t")
            if not SQL_RE.fullmatch(name) or not HASH_RE.fullmatch(checksum) or name in rows:
                return None
            rows[name] = checksum
        return rows
    except (UpgradeError, UnicodeError, ValueError):
        return None


def _collect_managed(install, bundle):
    managed = set(bundle.files) - RETAINED_CONFIG
    # These are the only live paths that can be removed or restored by rollback.
    for name in (REQUIRED_FILES | OPTIONAL_FILES) - RETAINED_CONFIG:
        path = _target(install, name)
        if path.exists():
            managed.add(name)
    resources = _safe_directory(install / "backend/resources")
    for directory, directories, files in os.walk(resources, followlinks=False):
        for name in directories:
            _safe_directory(Path(directory) / name)
        for name in files:
            relative = (Path(directory) / name).relative_to(install).as_posix()
            if not _allowed_file(relative):
                raise UpgradeError("Managed resource directory contains an unsafe file.")
            _target(install, relative)
            managed.add(relative)
    for name in managed:
        _target(install, name)
    return sorted(managed)


def _copy_app_data(source, destination):
    _safe_directory(source)
    destination.mkdir(mode=0o700)
    for directory, directories, files in os.walk(source, followlinks=False):
        relative = Path(directory).relative_to(source)
        target_dir = destination / relative
        for name in directories:
            _safe_directory(Path(directory) / name)
            (target_dir / name).mkdir(mode=0o700)
        for name in files:
            source_file = Path(directory) / name
            # Open without following links, and stream potentially large files.
            _atomic_file(target_dir / name, source=source_file)


def _save_state(backup, state):
    _atomic_file(backup / "recovery.json", data=(json.dumps(state, sort_keys=True, indent=2) + "\n").encode())


def _backup(config, runner, managed, old, bundle, snapshot, eligible):
    backups = _private_directory(config.backups_dir)
    backup = Path(tempfile.mkdtemp(prefix="before-" + bundle.version + "-", dir=str(backups)))
    state = {"status": "preparing", "previous_app": old, "target_version": bundle.version,
             "target_commit": bundle.commit, "migration_before": snapshot,
             "automatic_rollback_eligible": eligible, "managed": {}, "changed": []}
    _save_state(backup, state)
    for name in sorted(set(managed) | RETAINED_CONFIG | {"deploy/.env"}):
        # .env is allowed only as an explicitly named backup, never an archive path.
        source = config.install_dir / "deploy/.env" if name == "deploy/.env" else _target(config.install_dir, name)
        if not source.exists():
            state["managed"][name] = None
            continue
        info = source.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise UpgradeError("Cannot back up a linked or special live file.")
        target = backup / "files" / name
        _safe_directory(target.parent, create=True, mode=0o700)
        _atomic_file(target, source=source)
        state["managed"][name] = {"mode": stat.S_IMODE(info.st_mode), "uid": info.st_uid, "gid": info.st_gid}
    _copy_app_data(config.install_dir / "deploy/car-data/app", backup / "app")
    dump = backup / "database.dump"
    with dump.open("xb") as handle:
        os.fchmod(handle.fileno(), 0o600)
        _run(runner, _compose(config, "exec", "-T", "postgres", "pg_dump", "-U", "sub2api", "-d", "sub2api", "-Fc"),
             "Database backup", stdout=handle, timeout=1800)
        handle.flush()
        os.fsync(handle.fileno())
    if dump.stat().st_size == 0:
        raise UpgradeError("Database backup is empty.")
    with _open_regular(dump) as handle:
        _run(runner, _compose(config, "exec", "-T", "postgres", "pg_restore", "--list"),
             "Validate database backup", stdin=handle, stdout=subprocess.DEVNULL, timeout=300)
    state["status"] = "prepared"
    _save_state(backup, state)
    return backup, state


def _app_up(config, runner, validate=True, reconfigure=False):
    if validate:
        _run(runner, _compose(config, "config", "--quiet"), "Validate active Compose configuration")
    # A replaced host bind directory leaves the Compose config hash unchanged.
    # Internal bootstrap repair must retire that existing container explicitly.
    recreate = ["--force-recreate"] if reconfigure else []
    _run(runner, _compose(config, "up", "-d", "--no-deps", "--no-build", "--pull", "never", "--timeout", "30",
                         *recreate, "--wait", "--wait-timeout", "120", "sub2api"), "Start app", timeout=210)


def _stop_app(config, runner):
    _run(runner, _compose(config, "stop", "--timeout", "30", "sub2api"), "Stop failed app", timeout=60)


def _restore_managed(config, backup, state):
    for name in reversed(state["changed"]):
        target = config.install_dir / "deploy/.env" if name == "deploy/.env" else _target(config.install_dir, name, create_parents=True)
        metadata = state["managed"][name]
        if metadata is None:
            # Never remove arbitrary directories or any persistent data.
            if target.exists():
                if not stat.S_ISREG(target.lstat().st_mode):
                    raise UpgradeError("Rollback encountered an unsafe managed path.")
                target.unlink()
                _fsync_directory(target.parent)
        else:
            _atomic_file(target, source=backup / "files" / name, mode=metadata["mode"], owner=(metadata["uid"], metadata["gid"]))


def _verify_image(runner, bundle, prepared_image=None):
    """Verify the mutable tag and then inspect the immutable image contents."""
    image_tag = "sub2api-car:" + bundle.version
    image_result = _run(runner, ["docker", "image", "inspect", "--format", "{{.Id}}", image_tag], "Inspect new image")
    image_id = image_result.stdout.decode("ascii").strip()
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", image_id):
        raise UpgradeError("Cannot identify the newly built image.")
    if prepared_image is not None and image_id != prepared_image.image_id:
        raise UpgradeError("Prepared image tag no longer identifies the verified image.")
    image_version = _version_output(_run(runner, ["docker", "run", "--rm", "--network", "none", "--read-only",
                                                                 "--entrypoint", "/app/sub2api", image_id, "--version"], "Verify new image", timeout=60))
    if image_version != (bundle.version, bundle.commit):
        raise UpgradeError("Built image version or commit does not match the verified bundle.")
    return image_id


def upgrade(archive, config=None, sha256=None, runner=None, report=print, prepared_image=None,
            reconfigure=False, _lock=None):
    """Core transaction; CLI enforces root, while tests inject a command runner."""
    config = config or UpgradeConfig()
    config = UpgradeConfig(*(Path(os.path.abspath(p)) for p in (config.install_dir, config.updates_dir, config.backups_dir)))
    paths = (config.install_dir, config.updates_dir, config.backups_dir)
    for index, first in enumerate(paths):
        for second in paths[index + 1:]:
            if first == second or first in second.parents or second in first.parents:
                raise UpgradeError("Installation, staging, and backup directories must not overlap.")
    runner = runner or SubprocessRunner()
    stage, release, bundle = _stage_archive(archive, sha256, config.updates_dir)
    if prepared_image is not None:
        if (not isinstance(prepared_image, PreparedImage)
                or (prepared_image.archive_sha256, prepared_image.version, prepared_image.commit)
                    != (bundle.sha256, bundle.version, bundle.commit)
                or not isinstance(prepared_image.image_id, str)
                or not re.fullmatch(r"sha256:[a-f0-9]{64}", prepared_image.image_id)):
            raise UpgradeError("Prepared image metadata does not match the verified bundle.")
    report("Verified private bundle {} (commit {}); staging: {}".format(bundle.version, bundle.commit, stage))
    if _lock is not None:
        if not isinstance(_lock, _HeldInstallationLock):
            raise UpgradeError("Invalid installation lock handoff.")
        _lock.validate(config.install_dir)
    with (installation_lock(config.install_dir) if _lock is None else contextlib.nullcontext(_lock)):
        _safe_directory(config.install_dir / "deploy")
        env_path = config.install_dir / "deploy/.env"
        with _open_regular(env_path) as handle:
            env_info = os.fstat(handle.fileno())
            if env_info.st_nlink != 1 or stat.S_IMODE(env_info.st_mode) & 0o077:
                raise UpgradeError("The persistent .env must be a private regular file (mode 0600 or stricter).")
            original_env = handle.read(MAX_METADATA_BYTES + 1)
        if len(original_env) > MAX_METADATA_BYTES:
            raise UpgradeError("The persistent .env is unexpectedly large.")
        old_env_version, candidate_env = _env_version(original_env, bundle.version)
        for name in (RETAINED_CONFIG - OPTIONAL_CONFIG) | {"sub2api", "BUILD_VERSION", "BUILD_COMMIT"}:
            if not _target(config.install_dir, name).exists():
                raise UpgradeError("The existing installation is missing a required managed file.")
        for name in OPTIONAL_CONFIG:
            _target(config.install_dir, name)
        managed = _collect_managed(config.install_dir, bundle)
        old = _running_app(config, runner)
        if old["version"] != old_env_version:
            raise UpgradeError("The running version and persistent .env disagree; reconcile them before upgrading.")
        if prepared_image is not None:
            # Check under the installation lock, not only in the web service:
            # a manual upgrade may have advanced the app since preparation.
            def version_order(value):
                core, car = value.split("-car.")
                return tuple(int(part) for part in core.split(".")) + (int(car),)
            if version_order(old["version"]) > version_order(bundle.version):
                raise UpgradeError("The prepared version is older than the running application.")
            if old["version"] == bundle.version and old["image_id"] != prepared_image.image_id:
                raise UpgradeError("The running version does not use the prepared image identity.")
            report("Revalidating the previously prepared app image before any no-op or activation.")
            image_id = _verify_image(runner, bundle, prepared_image)
        if old["version"] == bundle.version:
            if old["commit"] != bundle.commit:
                raise UpgradeError("Refusing to reuse the running version tag for a different commit.")
            if not old["healthy"]:
                raise UpgradeError("This version is already running but is unhealthy; no files were changed.")
            if not reconfigure:
                report("This exact version and commit are already running and healthy; no upgrade needed.")
                return UpgradeResult(bundle.version, bundle.commit, None, stage, True)
        snapshot = _migration_snapshot(config, runner)
        eligible = (bundle.migrations is not None and snapshot is not None
                    and all(snapshot.get(name) == digest for name, digest in bundle.migrations.items()))
        if not eligible:
            report("Migration preflight is pending, changed, or unknown; automatic application rollback is disabled.")
        image_tag = "sub2api-car:" + bundle.version
        if prepared_image is None and old["version"] == bundle.version:
            image_id = _verify_image(runner, bundle)
            if image_id != old["image_id"]:
                raise UpgradeError("The running version's image tag changed before reconfiguration.")
        elif prepared_image is None:
            report("Building and verifying the new app image while the current app keeps running.")
            _run(runner, ["docker", "build", "-f", "Dockerfile.goreleaser", "-t", image_tag, "."], "Image build", cwd=release, timeout=1800)
            image_id = _verify_image(runner, bundle)
        candidate_path = stage / "candidate.env"
        _atomic_file(candidate_path, data=candidate_env)
        _run(runner, _compose(config, "config", "--quiet", env_file=candidate_path), "Validate candidate Compose configuration")
        report("Creating a private database, configuration, program, and app-data backup.")
        backup, state = _backup(config, runner, managed, old, bundle, snapshot, eligible)
        report("Verified backup: {}".format(backup))
        # Abort without live mutation if another actor changed the source configuration.
        with _open_regular(env_path) as handle:
            if handle.read(MAX_METADATA_BYTES + 1) != original_env:
                raise UpgradeError("The persistent .env changed during preflight; no live files were replaced.")
        before_switch = _migration_snapshot(config, runner)
        if snapshot != before_switch:
            raise UpgradeError("Migration ledger changed during preflight; no live files were replaced.")
        state["status"] = "switching"
        _save_state(backup, state)
        attempted_start = False
        try:
            for name in managed:
                target = _target(config.install_dir, name, create_parents=True)
                state["changed"].append(name)
                _save_state(backup, state)
                if name not in bundle.files:
                    if target.exists():
                        target.unlink()
                        _fsync_directory(target.parent)
                    continue
                metadata = state["managed"][name]
                mode = 0o755 if name == "sub2api" or name.endswith(".sh") else 0o644
                owner = (metadata["uid"], metadata["gid"]) if metadata else None
                _atomic_file(target, source=release / name, mode=mode, owner=owner)
            state["changed"].append("deploy/.env")
            _save_state(backup, state)
            _atomic_file(env_path, data=candidate_env, mode=stat.S_IMODE(env_info.st_mode), owner=(env_info.st_uid, env_info.st_gid))
            _run(runner, _compose(config, "config", "--quiet"), "Validate active Compose configuration")
            attempted_start = True
            _app_up(config, runner, validate=False, reconfigure=reconfigure)
            running = _running_app(config, runner)
            if ((running["version"], running["commit"], running["image_id"]) != (bundle.version, bundle.commit, image_id)
                    or not running["healthy"]):
                raise UpgradeError("New running app failed exact version, commit, image, or health verification.")
            state["migration_after"] = _migration_snapshot(config, runner)
            state["status"] = "complete"
            _save_state(backup, state)
            report("Upgrade complete: {} (commit {}), app healthy. Backup retained: {}".format(bundle.version, bundle.commit, backup))
            return UpgradeResult(bundle.version, bundle.commit, backup, stage)
        except (UpgradeError, OSError, UnicodeError, KeyboardInterrupt) as failure:
            # Freeze the failed app before checking the ledger: migrations must not
            # race the safety decision, including nontransactional migrations.
            cause = str(failure) if isinstance(failure, UpgradeError) else "Upgrade interrupted or filesystem operation failed."
            stopped = not attempted_start
            recovered = False
            try:
                if attempted_start:
                    _stop_app(config, runner)
                    stopped = True
                after = _migration_snapshot(config, runner) if stopped else None
                state["migration_after"] = after
                if not attempted_start or (eligible and stopped and after == snapshot):
                    _restore_managed(config, backup, state)
                    if attempted_start:
                        _run(runner, ["docker", "tag", old["image_id"], old["image_tag"]], "Restore previous image tag")
                        _app_up(config, runner, reconfigure=reconfigure)
                    restored = _running_app(config, runner)
                    if ((attempted_start and not restored["healthy"])
                            or any(restored[key] != old[key] for key in ("version", "commit", "image_id"))):
                        raise UpgradeError("Previous app did not pass rollback health and identity verification.")
                    state["status"] = "rolled_back"
                    _save_state(backup, state)
                    recovered = True
                else:
                    state["status"] = "manual_recovery_required"
                    state["app_stop_confirmed"] = stopped
                    _save_state(backup, state)
            except (UpgradeError, OSError, UnicodeError, KeyboardInterrupt) as recovery_error:
                state["status"] = "manual_recovery_required"
                state["recovery_error"] = (str(recovery_error) if isinstance(recovery_error, UpgradeError) else
                                           "Rollback interrupted or filesystem operation failed.")
                if attempted_start:
                    stopped = False
                    try:
                        _stop_app(config, runner)
                        stopped = True
                    except (UpgradeError, OSError, KeyboardInterrupt):
                        pass
                state["app_stop_confirmed"] = stopped if attempted_start else False
                try:
                    _save_state(backup, state)
                except (OSError, KeyboardInterrupt):
                    # Even repeated interruption or a full disk cannot remove the
                    # prepared recovery.json and original private backup files.
                    pass
            if recovered:
                outcome = ("Application rolled back and verified healthy" if attempted_start else
                           "Live files restored; previous app kept running without restart")
                raise UpgradeError(cause + " " + outcome + "; database was not restored. Backup: " + str(backup)) from None
            raise UpgradeError(cause + " Automatic application rollback was not completed; manual recovery required. "
                               "The database was not restored. Keep the current files and backup at " + str(backup) +
                               "; inspect recovery.json and reconcile the migration ledger before restarting the app. "
                               + ("The previous app was not stopped or restarted." if not attempted_start else
                                  "The app was stopped." if stopped else
                                  "Stopping the app could not be confirmed; stop only sub2api before recovery.")) from None



@contextlib.contextmanager
def _termination_handler():
    previous = signal.getsignal(signal.SIGTERM)
    def interrupt(signum, frame):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupt)
    try:
        yield
    finally:
        signal.signal(signal.SIGTERM, previous)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("--sha256")
    parser.add_argument("--check-only", action="store_true")
    parser.add_argument("--install-dir", type=Path, default=Path("/opt/sub2api-car"))
    parser.add_argument("--updates-dir", type=Path, default=Path("/opt/sub2api-car-updates"))
    parser.add_argument("--backups-dir", type=Path, default=Path("/opt/sub2api-car-backups"))
    args = parser.parse_args(argv)
    try:
        if args.check_only:
            bundle = verify_archive(args.archive, args.sha256)
            print("Verified {} (commit {}).".format(bundle.version, bundle.commit))
            if bundle.migrations is None:
                print("Legacy bundle: automatic application rollback is unavailable.")
        else:
            if os.geteuid() != 0:
                raise UpgradeError("An actual upgrade requires root; use sudo or --check-only.")
            with _termination_handler():
                upgrade(args.archive, UpgradeConfig(args.install_dir, args.updates_dir, args.backups_dir), args.sha256)
        return 0
    except UpgradeError as error:
        print("Upgrade failed: {}".format(error), file=sys.stderr)
        return 1
    except (OSError, UnicodeError, KeyboardInterrupt):
        print("Upgrade interrupted or filesystem operation failed; inspect retained staging and backups before retrying.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
