#!/usr/bin/env python3
"""Bounded, unauthenticated downloads from the fixed public car release source.

The adjacent checksum establishes transport integrity. The repository and exact
release/asset paths establish the configured source; no caller can select a URL.
Python 3.9+ standard library only. Network boundaries are injectable for tests.
"""
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

LATEST_URL = "https://api.github.com/repos/CLccxx/sub2api-car/releases/latest"
ASSET_BASE = "https://github.com/CLccxx/sub2api-car/releases/download/"
ALLOWED_HOSTS = frozenset({"github.com", "api.github.com", "objects.githubusercontent.com",
                           "release-assets.githubusercontent.com"})
MAX_RELEASE_BYTES = 2 * 1024 * 1024
MAX_FETCH_REQUEST_BYTES = 32 * 1024
MAX_CHECKSUM_BYTES = 4096
MAX_DOWNLOAD_BYTES = 1024 * 1024 * 1024
CHUNK_BYTES = 128 * 1024
SOCKET_TIMEOUT = 30
METADATA_TIMEOUT = 30
DOWNLOAD_TIMEOUT = 900
VERSION_RE = re.compile(r"([0-9]+)\.([0-9]+)\.([0-9]+)-car\.([0-9]+)\Z")


class ReleaseError(Exception):
    """A fixed safe message, never an upstream body or a signed redirect URL."""


def version_key(version):
    match = VERSION_RE.fullmatch(version) if isinstance(version, str) and len(version) <= 128 else None
    if match is None:
        raise ReleaseError("A valid custom release version is required.")
    return tuple(int(part) for part in match.groups())


def _official_url(url):
    try:
        if not isinstance(url, str) or len(url) > 16384 or any(ord(c) <= 32 or ord(c) == 127 for c in url):
            raise ValueError()
        parsed = urllib.parse.urlsplit(url)
        if (parsed.scheme != "https" or parsed.hostname not in ALLOWED_HOSTS
                or parsed.username is not None or parsed.password is not None
                or parsed.port not in (None, 443) or "#" in url or "\\" in url):
            raise ValueError()
        return parsed
    except (ValueError, TypeError):
        raise ReleaseError("Release download URL is not permitted.") from None


class GitHubRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Validate each hop and never forward credentials to a redirected CDN."""
    max_redirections = 5
    max_repeats = 2

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        _official_url(newurl)
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if redirected is not None:
            for name in tuple(redirected.headers) + tuple(redirected.unredirected_hdrs):
                if name.lower() in {"authorization", "proxy-authorization", "cookie", "host"}:
                    redirected.remove_header(name)
        return redirected


def _unique_json(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise ReleaseError("Release metadata is invalid.")
        result[name] = value
    return result


@dataclass(frozen=True)
class Release:
    version: str
    filename: str
    archive_url: str
    checksum_url: str


@dataclass(frozen=True)
class DownloadedRelease:
    version: str
    archive: Path
    sha256: str


class ReleaseClient:
    def __init__(self, opener=None, clock=time.monotonic):
        # An injected opener is an offline test boundary. Real network I/O runs
        # in a fresh interpreter so even blocking DNS/TLS/header reads can be
        # cancelled and reaped without leaking threads in the host daemon.
        self.opener = opener
        self.clock = clock

    def _read(self, url, limit, seconds, output=None):
        if self.opener is not None:
            return self._read_in_process(url, limit, seconds, output)
        _official_url(url)
        deadline = self.clock() + seconds
        request = json.dumps({"url": url, "limit": limit, "seconds": seconds}).encode()
        command = [sys.executable, "-I", "-B", str(Path(__file__).resolve()), "--fetch-worker"]
        worker = None
        try:
            # This file is unlinked immediately, owned by this process and 0600.
            # Never read a pipe before the child's absolute deadline is enforced.
            with tempfile.TemporaryFile(mode="w+b") as incoming:
                worker = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=incoming, stderr=subprocess.DEVNULL,
                                          env={"PATH": os.defpath, "LANG": "C.UTF-8"}, start_new_session=True)
                remaining = deadline - self.clock()
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(command, seconds)
                worker.communicate(input=request, timeout=remaining)
                if worker.returncode:
                    raise ReleaseError("Release request failed.")
                incoming.seek(0)
                digest, chunks, length = hashlib.sha256(), [], 0
                while True:
                    if self.clock() >= deadline:
                        raise ReleaseError("Release request exceeded its time limit.")
                    data = incoming.read(min(CHUNK_BYTES, limit + 1 - length))
                    if not data:
                        break
                    length += len(data)
                    if length > limit:
                        raise ReleaseError("Release response exceeds its size limit.")
                    digest.update(data)
                    if output is None:
                        chunks.append(data)
                    else:
                        output.write(data)
                if self.clock() >= deadline:
                    raise ReleaseError("Release request exceeded its time limit.")
                return b"".join(chunks) if output is None else digest.hexdigest()
        except subprocess.TimeoutExpired:
            raise ReleaseError("Release request exceeded its time limit.") from None
        except (OSError, subprocess.SubprocessError):
            raise ReleaseError("Release request failed.") from None
        finally:
            if worker is not None:
                if worker.poll() is None:
                    worker.kill()
                # Reap on timeout, caller interruption, early failure and success.
                worker.wait()

    def _read_in_process(self, url, limit, seconds, output=None):
        _official_url(url)
        deadline = self.clock() + seconds
        request = urllib.request.Request(url, headers={"User-Agent": "sub2api-car-updater/1",
                                                       "Accept": "application/vnd.github+json" if url == LATEST_URL else "application/octet-stream"})
        digest, chunks, length = hashlib.sha256(), [], 0
        try:
            with self.opener.open(request, timeout=min(SOCKET_TIMEOUT, seconds)) as response:
                _official_url(response.geturl())
                if response.status != 200:
                    raise ReleaseError("Release request failed.")
                declared = response.headers.get("Content-Length")
                if declared is not None:
                    if not re.fullmatch(r"[0-9]+", declared) or len(declared) > 12 or int(declared) > limit:
                        raise ReleaseError("Release response exceeds its size limit.")
                    declared = int(declared)
                while True:
                    if self.clock() >= deadline:
                        raise ReleaseError("Release request exceeded its time limit.")
                    # HTTPResponse.read(n) may wait for all n bytes while a
                    # trickling peer avoids each socket timeout. read1 performs
                    # at most one raw read, so the total deadline is rechecked.
                    read_chunk = getattr(response, "read1", response.read)
                    data = read_chunk(min(CHUNK_BYTES, limit + 1 - length))
                    if self.clock() >= deadline:
                        raise ReleaseError("Release request exceeded its time limit.")
                    if not data:
                        break
                    length += len(data)
                    if length > limit:
                        raise ReleaseError("Release response exceeds its size limit.")
                    digest.update(data)
                    if output is None:
                        chunks.append(data)
                    else:
                        output.write(data)
                if declared is not None and declared != length:
                    raise ReleaseError("Release response was incomplete.")
                return b"".join(chunks) if output is None else digest.hexdigest()
        except (OSError, ValueError, urllib.error.URLError):
            raise ReleaseError("Release request failed.") from None

    def latest(self, requested_version):
        version_key(requested_version)
        data = self._read(LATEST_URL, MAX_RELEASE_BYTES, METADATA_TIMEOUT)
        try:
            info = json.loads(data, object_pairs_hook=_unique_json)
            if (not isinstance(info, dict) or info.get("draft") is not False
                    or info.get("prerelease") is not False or info.get("tag_name") != "v" + requested_version
                    or not isinstance(info.get("assets"), list)):
                raise ValueError()
            filename = "sub2api-car-" + requested_version + "-linux-amd64.tar.gz"
            assets = {}
            for asset in info["assets"]:
                if not isinstance(asset, dict):
                    raise ValueError()
                name = asset.get("name")
                if name not in (filename, filename + ".sha256"):
                    continue
                if name in assets:
                    raise ValueError()
                url = asset.get("browser_download_url")
                parsed = _official_url(url)
                if (parsed.hostname != "github.com" or parsed.query
                        or parsed.path != "/CLccxx/sub2api-car/releases/download/v" + requested_version + "/" + name):
                    raise ValueError()
                assets[name] = url
            if set(assets) != {filename, filename + ".sha256"}:
                raise ValueError()
            return Release(requested_version, filename, assets[filename], assets[filename + ".sha256"])
        except (ValueError, TypeError, UnicodeError):
            raise ReleaseError("Latest release does not contain the requested complete stable bundle.") from None

    def download_latest(self, requested_version, directory):
        release = self.latest(requested_version)
        data = self._read(release.checksum_url, MAX_CHECKSUM_BYTES, METADATA_TIMEOUT)
        match = re.fullmatch(rb"([a-f0-9]{64})  ([^\r\n]+)\n?", data)
        if match is None or match[2] != release.filename.encode("ascii"):
            raise ReleaseError("Release checksum file is invalid.")
        checksum = match[1].decode("ascii")
        archive = Path(directory) / release.filename
        created = False
        try:
            fd = os.open(str(archive), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            created = True
            with os.fdopen(fd, "wb") as output:
                received = self._read(release.archive_url, MAX_DOWNLOAD_BYTES, DOWNLOAD_TIMEOUT, output)
                output.flush()
                os.fsync(output.fileno())
            if received != checksum:
                raise ReleaseError("Release archive checksum does not match.")
            return DownloadedRelease(release.version, archive, checksum)
        except (OSError, ReleaseError):
            if created:
                archive.unlink(missing_ok=True)
            raise ReleaseError("Unable to download and verify the release archive.") from None


def _fetch_worker(input_stream=None, output=None):
    """Private pipe-based transport worker; it has no service action or socket."""
    input_stream = sys.stdin.buffer if input_stream is None else input_stream
    output = sys.stdout.buffer if output is None else output
    try:
        data = input_stream.read(MAX_FETCH_REQUEST_BYTES + 1)
        if len(data) > MAX_FETCH_REQUEST_BYTES:
            return 1
        request = json.loads(data, object_pairs_hook=_unique_json)
        if (not isinstance(request, dict) or set(request) != {"url", "limit", "seconds"}
                or type(request["limit"]) is not int or not 0 < request["limit"] <= MAX_DOWNLOAD_BYTES
                or type(request["seconds"]) not in {int, float} or not 0 < request["seconds"] <= DOWNLOAD_TIMEOUT):
            return 1
        _official_url(request["url"])
        client = ReleaseClient(opener=urllib.request.build_opener(GitHubRedirectHandler()))
        client._read_in_process(request["url"], request["limit"], request["seconds"], output)
        output.flush()
        return 0
    except (Exception, KeyboardInterrupt):
        # Only the parent emits a fixed error. URLs, signed redirects, response
        # bodies and exception details never enter the daemon's journal.
        return 1


if __name__ == "__main__":
    sys.exit(_fetch_worker() if sys.argv[1:] == ["--fetch-worker"] else 2)
