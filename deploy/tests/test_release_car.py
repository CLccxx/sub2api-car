"""Offline tests for the fixed GitHub release source and bounded downloads."""
import hashlib
import http.client
import os
import io
import json
from pathlib import Path
import stat
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
import urllib.request
import urllib.response

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    import car_release as release
except ModuleNotFoundError:
    release = None

VERSION = "0.2.17-car.7"
FILENAME = "sub2api-car-" + VERSION + "-linux-amd64.tar.gz"
BASE = "https://github.com/CLccxx/sub2api-car/releases/download/v" + VERSION + "/"
LATEST = "https://api.github.com/repos/CLccxx/sub2api-car/releases/latest"


def metadata():
    return {"tag_name": "v" + VERSION, "draft": False, "prerelease": False,
            "assets": [{"name": FILENAME, "browser_download_url": BASE + FILENAME},
                       {"name": FILENAME + ".sha256", "browser_download_url": BASE + FILENAME + ".sha256"},
                       {"name": "source.zip", "browser_download_url": "https://irrelevant.example/source.zip"}]}


class Response(io.BytesIO):
    def __init__(self, data, url, headers=None):
        super().__init__(data)
        self.url = url
        self.headers = headers or {}
        self.status = 200
    def geturl(self):
        return self.url


class FakeHTTP:
    def __init__(self, body=b"archive"):
        self.body = body
        self.info = metadata()
        self.checksum = (hashlib.sha256(body).hexdigest() + "  " + FILENAME + "\n").encode()
        self.calls = []
        self.headers = {}
    def open(self, request, timeout=None):
        self.calls.append((request.full_url, timeout, dict(request.header_items())))
        if request.full_url == LATEST:
            data = json.dumps(self.info).encode()
        elif request.full_url == BASE + FILENAME + ".sha256":
            data = self.checksum
        elif request.full_url == BASE + FILENAME:
            data = self.body
        else:
            raise AssertionError("Unexpected URL")
        return Response(data, request.full_url, self.headers)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(release, "The fixed-source release client must exist")
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.http = FakeHTTP()
        self.client = release.ReleaseClient(opener=self.http)

    def test_custom_version_order_is_numeric_including_core(self):
        self.assertLess(release.version_key("0.2.17-car.9"), release.version_key("0.2.17-car.10"))
        self.assertLess(release.version_key("0.2.17-car.999"), release.version_key("0.2.18-car.1"))
        for value in ("v0.2.17-car.7", "1.2.3", "1.2.3-car.x", "1.2.3-car.-1", "1.2.3-car.7\n", "1.2.3-car.7;id", "1" * 200):
            with self.subTest(value=value), self.assertRaises(release.ReleaseError):
                release.version_key(value)

    def test_latest_selects_exact_required_assets_and_ignores_unrelated_assets(self):
        found = self.client.latest(VERSION)
        self.assertEqual((found.version, found.filename, found.archive_url, found.checksum_url),
                         (VERSION, FILENAME, BASE + FILENAME, BASE + FILENAME + ".sha256"))
        self.assertEqual([call[0] for call in self.http.calls], [LATEST])

    def test_metadata_requires_stable_requested_release_and_unique_assets(self):
        for change in ("draft", "prerelease", "wrong_tag", "no_v", "missing", "duplicate", "wrong_type"):
            self.http.info = metadata()
            if change in ("draft", "prerelease"):
                self.http.info[change] = True
            elif change == "wrong_tag":
                self.http.info["tag_name"] = "v0.2.17-car.8"
            elif change == "no_v":
                self.http.info["tag_name"] = VERSION
            elif change == "missing":
                self.http.info["assets"].pop(0)
            elif change == "duplicate":
                self.http.info["assets"].append(self.http.info["assets"][0])
            else:
                self.http.info["draft"] = 0
            with self.subTest(change=change), self.assertRaises(release.ReleaseError):
                self.client.latest(VERSION)

    def test_initial_asset_url_must_bind_exact_host_repository_tag_and_filename(self):
        invalid = ["http://github.com/CLccxx/sub2api-car/releases/download/v" + VERSION + "/" + FILENAME,
                   BASE.replace("github.com", "github.com.evil.test") + FILENAME,
                   BASE.replace("CLccxx", "attacker") + FILENAME,
                   BASE.replace("v" + VERSION, "v0.2.17-car.6") + FILENAME,
                   BASE + "other.tar.gz", BASE + FILENAME + "?download=1", BASE + FILENAME + "#fragment", BASE + FILENAME + "#",
                   BASE.replace("github.com", "user@github.com") + FILENAME,
                   BASE.replace("github.com", "github.com:444") + FILENAME,
                   BASE + "%2e%2e/" + FILENAME]
        for url in invalid:
            self.http.info = metadata()
            self.http.info["assets"][0]["browser_download_url"] = url
            with self.subTest(url=url), self.assertRaises(release.ReleaseError):
                self.client.latest(VERSION)

    def test_download_streams_private_file_and_enforces_checksum(self):
        result = self.client.download_latest(VERSION, self.root)
        self.assertEqual(result.archive.read_bytes(), self.http.body)
        self.assertEqual(result.sha256, hashlib.sha256(self.http.body).hexdigest())
        self.assertEqual(stat.S_IMODE(result.archive.stat().st_mode), 0o600)
        self.assertEqual(result.version, VERSION)
        self.assertEqual([c[0] for c in self.http.calls], [LATEST, BASE + FILENAME + ".sha256", BASE + FILENAME])
        self.assertTrue(all(0 < c[1] <= 30 for c in self.http.calls))

    def test_bad_checksum_or_wrong_sidecar_filename_never_yields_artifact(self):
        for sidecar in (("0" * 64 + "  " + FILENAME + "\n").encode(),
                        (hashlib.sha256(b"archive").hexdigest() + "  other.tar.gz\n").encode(),
                        b"not a checksum", b"f" * 4097):
            self.http.checksum = sidecar
            with self.subTest(sidecar=sidecar[:20]), self.assertRaises(release.ReleaseError):
                self.client.download_latest(VERSION, self.root)
            self.assertEqual(list(self.root.iterdir()), [])

    def test_all_response_types_have_size_limits(self):
        for limit in ("MAX_RELEASE_BYTES", "MAX_CHECKSUM_BYTES", "MAX_DOWNLOAD_BYTES"):
            with self.subTest(limit=limit), mock.patch.object(release, limit, 3), self.assertRaises(release.ReleaseError):
                self.client.download_latest(VERSION, self.root)
            self.assertEqual(list(self.root.iterdir()), [])

    def test_declared_size_and_truncated_response_are_rejected(self):
        for value in ("-1", "not-a-size", "999999999999", "500"):
            self.http.headers = {"Content-Length": value}
            with self.subTest(value=value), self.assertRaises(release.ReleaseError):
                self.client.latest(VERSION)

    def test_metadata_duplicate_keys_are_rejected(self):
        class DuplicateHTTP:
            def open(self, request, timeout=None):
                data = json.dumps(metadata())[:-1] + ', "tag_name": "v' + VERSION + '"}'
                return Response(data.encode(), LATEST)
        with self.assertRaises(release.ReleaseError):
            release.ReleaseClient(opener=DuplicateHTTP()).latest(VERSION)

    def test_total_deadline_is_enforced_between_reads(self):
        class SlowHTTP(FakeHTTP):
            def open(self, request, timeout=None):
                ticks[0] += 40
                return super().open(request, timeout)
        ticks = [0]
        with self.assertRaises(release.ReleaseError):
            release.ReleaseClient(opener=SlowHTTP(), clock=lambda: ticks[0]).latest(VERSION)

    def test_streaming_reads_check_deadline_without_waiting_for_full_chunk(self):
        ticks = [0]
        class TrickleResponse(Response):
            def read(self, count=-1):
                # A read(n) can wait for n bytes while a slow peer keeps each
                # individual socket read under its timeout.
                ticks[0] += 1000
                return super().read(count)
            def read1(self, count=-1):
                ticks[0] += 1
                return io.BytesIO.read(self, min(count, 1))
        class TrickleHTTP:
            def open(self, request, timeout=None):
                return TrickleResponse(json.dumps(metadata()).encode(), LATEST)
        with self.assertRaises(release.ReleaseError):
            release.ReleaseClient(opener=TrickleHTTP(), clock=lambda: ticks[0]).latest(VERSION)
        self.assertLessEqual(ticks[0], 31)

    def test_real_http_headers_have_a_parent_deadline_and_timed_out_children_are_reaped(self):
        # The fallback opener reproduces the old blocking-header path. The child
        # factory exercises the production deadline wrapper without Internet.
        threads, channels, children = [], [], []
        stop = threading.Event()
        class SlowHeaders:
            def open(self, request, timeout=None):
                client, server = socket.socketpair()
                channels.extend((client, server))
                client.settimeout(timeout)
                def send():
                    data = b"HTTP/1.1 200 OK\r\nX-Slow: " + b"x" * 160 + b"\r\nContent-Length: 2\r\n\r\n{}"
                    try:
                        for value in data:
                            if stop.wait(0.005):
                                break
                            server.sendall(bytes((value,)))
                    except OSError:
                        pass
                thread = threading.Thread(target=send)
                threads.append(thread)
                thread.start()
                response = http.client.HTTPResponse(client)
                response.begin()
                return urllib.response.addinfourl(response, response.headers, request.full_url, 200)
        child_code = r"""
import http.client, json, socket, sys, threading, time
from pathlib import Path
json.loads(sys.stdin.buffer.read())
client, server = socket.socketpair()
client.settimeout(2)
def send():
    data = b"HTTP/1.1 200 OK\r\nX-Slow: " + b"x" * 160 + b"\r\nContent-Length: 2\r\n\r\n{}"
    try:
        for value in data:
            server.sendall(bytes((value,)))
            time.sleep(0.005)
    except OSError:
        pass
threading.Thread(target=send, daemon=True).start()
Path(sys.argv[1]).write_text("reading headers")
response = http.client.HTTPResponse(client)
response.begin()
sys.stdout.buffer.write(response.read())
"""
        original_popen = subprocess.Popen
        def launch(command, **kwargs):
            self.assertEqual(stat.S_IMODE(os.fstat(kwargs["stdout"].fileno()).st_mode), 0o600)
            marker = self.root / ("headers-" + str(len(children)))
            child = original_popen([sys.executable, "-I", "-B", "-c", child_code, str(marker)], **kwargs)
            children.append((child, marker))
            return child
        before_threads = threading.active_count()
        try:
            with mock.patch.object(release, "subprocess", subprocess, create=True), mock.patch.object(subprocess, "Popen", launch), mock.patch.object(urllib.request, "build_opener", return_value=SlowHeaders()):
                for unused in range(3):
                    started = time.monotonic()
                    with self.assertRaises(release.ReleaseError):
                        release.ReleaseClient()._read(LATEST, 1024, 0.3)
                    self.assertLess(time.monotonic() - started, 0.8)
        finally:
            stop.set()
            for channel in channels:
                channel.close()
            for thread in threads:
                thread.join(2)
            for child, unused_marker in children:
                if child.poll() is None:
                    child.kill()
                    child.wait()
        self.assertEqual(len(children), 3)
        for child, marker in children:
            self.assertTrue(marker.exists(), "The child must actually start reading HTTP headers")
            self.assertIsNotNone(child.returncode)
            with self.assertRaises(ChildProcessError):
                os.waitpid(child.pid, os.WNOHANG)
        self.assertEqual(threading.active_count(), before_threads)

    def test_real_transfer_boundary_streams_verified_private_artifacts(self):
        payloads = {LATEST: json.dumps(metadata()), BASE + FILENAME: "archive",
                    BASE + FILENAME + ".sha256": self.http.checksum.decode()}
        code = "import json,sys; payloads=json.loads(sys.argv[1]); request=json.loads(sys.stdin.buffer.read()); sys.stdout.write(payloads[request['url']])"
        original_popen, children = subprocess.Popen, []
        def launch(command, **kwargs):
            self.assertEqual(stat.S_IMODE(os.fstat(kwargs["stdout"].fileno()).st_mode), 0o600)
            self.assertEqual(kwargs["stderr"], subprocess.DEVNULL)
            child = original_popen([sys.executable, "-I", "-B", "-c", code, json.dumps(payloads)], **kwargs)
            children.append(child)
            return child
        with mock.patch.object(release, "subprocess", subprocess, create=True), mock.patch.object(subprocess, "Popen", launch), mock.patch.object(urllib.request, "build_opener", return_value=self.http):
            downloaded = release.ReleaseClient().download_latest(VERSION, self.root)
        self.assertEqual(downloaded.archive.read_bytes(), b"archive")
        self.assertEqual(downloaded.sha256, hashlib.sha256(b"archive").hexdigest())
        self.assertEqual(stat.S_IMODE(downloaded.archive.stat().st_mode), 0o600)
        self.assertEqual(len(children), 3)
        self.assertTrue(all(child.returncode == 0 for child in children))

    def test_fetch_worker_validates_input_and_preserves_stream_bounds(self):
        self.assertTrue(hasattr(release, "_fetch_worker"), "The bounded HTTP worker entry point must exist")
        request = {"url": LATEST, "limit": 1024 * 1024, "seconds": 10}
        with mock.patch.object(urllib.request, "build_opener", return_value=self.http):
            output = io.BytesIO()
            self.assertEqual(release._fetch_worker(io.BytesIO(json.dumps(request).encode()), output), 0)
            self.assertEqual(json.loads(output.getvalue()), metadata())
            for change in ({"url": "https://evil.example/"}, {"limit": 1}, {"seconds": -1}, {"extra": "bad"}):
                invalid = dict(request, **change)
                self.assertEqual(release._fetch_worker(io.BytesIO(json.dumps(invalid).encode()), io.BytesIO()), 1)

    def test_redirects_allow_only_exact_official_https_hosts_and_strip_credentials(self):
        handler = release.GitHubRedirectHandler()
        request = urllib.request.Request(BASE + FILENAME, headers={"Authorization": "secret", "Cookie": "secret", "User-Agent": "test"})
        for host in ("github.com", "api.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"):
            redirected = handler.redirect_request(request, None, 302, "Found", {}, "https://" + host + "/asset?signature=example")
            self.assertFalse(redirected.has_header("Authorization"))
            self.assertFalse(redirected.has_header("Cookie"))
        for url in ("http://github.com/asset", "https://evil.example/asset", "https://github.com.evil.example/asset", "https://user@github.com/asset", "https://github.com:444/asset", "https://github.com/asset#fragment", "https://github.com/asset#"):
            with self.subTest(url=url), self.assertRaises(release.ReleaseError):
                handler.redirect_request(request, None, 302, "Found", {}, url)

    def test_unexpected_final_host_is_rejected_even_with_injected_opener(self):
        class HostSwapHTTP:
            def open(self, request, timeout=None):
                return Response(json.dumps(metadata()).encode(), "https://evil.example/release")
        with self.assertRaises(release.ReleaseError):
            release.ReleaseClient(opener=HostSwapHTTP()).latest(VERSION)


if __name__ == "__main__":
    unittest.main()
