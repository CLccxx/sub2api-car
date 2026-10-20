#!/usr/bin/env python3
"""Build a verifiable Linux amd64 carpool release using Python 3.9+ stdlib."""
from __future__ import annotations

import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from typing import Callable, Optional, Sequence

sys.dont_write_bytecode = True

RELEASE_FILES = (
    "Dockerfile.goreleaser", ".dockerignore",
    "deploy/docker-entrypoint.sh", "deploy/docker-compose.car.yml",
    "deploy/Caddyfile.car", "deploy/init-car.sh",
    "deploy/CARPOOL_DEPLOYMENT.md", "deploy/upgrade_car.py",
    "deploy/car_release.py", "deploy/car_update_service.py",
    "deploy/install_car_web_update.py", "deploy/sub2api-car-updater.service",
    "deploy/docker-compose.car-updater.yml",
)
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+-car\.[0-9]+\Z")
COMMIT = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})\Z")
# unicode.IsSpace, used by Go strings.TrimSpace. Python str.strip also removes
# U+001C..U+001F, which Go intentionally retains in a migration checksum.
GO_SPACE = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"


class PackageError(Exception):
    """The release was not published."""


@dataclass(frozen=True)
class Release:
    directory: Path
    archive: Path
    checksum: Path
    sha256: str
    commit: str


def run_command(args: Sequence[str], cwd: Path, capture: bool = False) -> str:
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith("VITE_")
                   and key not in {"NODE_ENV", "NODE_OPTIONS", "BABEL_ENV", "BROWSERSLIST_ENV"}}
    try:
        result = subprocess.run(
            list(args), cwd=cwd, check=True, text=True,
            stdout=subprocess.PIPE if capture else None,
            stderr=subprocess.STDOUT if capture else None,
            env=environment,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PackageError("命令执行失败：%s；未发布部署包。" % args[0]) from exc
    return result.stdout or ""


def git(repo: Path, *args: str) -> str:
    return run_command(["git", *args], repo, capture=True)


def clean_commit(repo: Path) -> str:
    if git(repo, "status", "--porcelain=v1", "--untracked-files=all"):
        raise PackageError("代码存在未提交的修改或文件，请先测试并提交，保持工作区干净。")
    commit = git(repo, "rev-parse", "HEAD").strip()
    if not COMMIT.fullmatch(commit):
        raise PackageError("无法确定完整的 Git 提交号。")
    return commit


def tracked_files(repo: Path, path: str) -> list[str]:
    return [name for name in git(repo, "ls-files", "-z", "--", path).split("\0") if name]


def safe_source(repo: Path, name: str) -> Path:
    path = repo / name
    if any(part.startswith(".env") or part == "car-data" for part in Path(name).parts):
        raise PackageError("发布文件清单包含私有配置或运行数据。")
    current = path
    while current != repo:
        if current.is_symlink():
            raise PackageError("发布文件不能使用符号链接：%s" % name)
        current = current.parent
    if not path.is_file():
        raise PackageError("缺少发布文件：%s" % name)
    return path


def migration_catalog(repo: Path, files: Optional[Sequence[str]] = None) -> dict:
    checksums = {}
    for name in files if files is not None else tracked_files(repo, "backend/migrations/*.sql"):
        if Path(name).parent != Path("backend/migrations"):
            continue
        source = safe_source(repo, name)
        # read_text normalizes CRLF; Go hashes the original line endings.
        content = source.read_bytes().decode("utf-8").strip(GO_SPACE)
        checksums[source.name] = hashlib.sha256(content.encode("utf-8")).hexdigest()
    if not checksums:
        raise PackageError("没有找到数据库迁移文件，无法生成迁移清单。")
    return {"format_version": 1, "files": dict(sorted(checksums.items()))}


def digest_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def source_snapshot(repo: Path, commit: str, destination: Path) -> None:
    """Build only the pinned commit, excluding ignored dotenv and build files."""
    archive = destination.parent / "source.tar"
    run_command(["git", "archive", "--format=tar", "--output=" + str(archive), commit], repo)
    destination.mkdir()
    with tarfile.open(archive, "r:") as tar:
        for item in tar:
            name = item.name.rstrip("/") if item.isdir() else item.name
            if (not name or str(PurePosixPath(name)) != name or name.startswith("/")
                    or ".." in PurePosixPath(name).parts or "\\" in name
                    or not (item.isdir() or item.isreg())):
                raise PackageError("提交中包含不支持的链接或源码路径。")
            target = destination / name
            if item.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(item) as incoming, target.open("xb") as output:
                    shutil.copyfileobj(incoming, output)
                target.chmod(0o755 if item.mode & 0o111 else 0o644)
    archive.unlink()


def package_release(
    repo: Path, version: str, output_dir: Optional[Path] = None,
    go_image: Optional[str] = None, test_app: bool = False,
    runner: Callable = run_command, report: Callable[[str], None] = print,
) -> Release:
    if not VERSION.fullmatch(version) or len(version) > 64:
        raise PackageError("版本格式必须为 X.Y.Z-car.N，例如 0.2.17-car.6。")
    repo = Path(repo).resolve()
    commit = clean_commit(repo)
    output = Path(output_dir).resolve() if output_dir else repo / "output"
    name = "sub2api-car-%s-linux-amd64" % version
    directory = output / name
    archive = output / (name + ".tar.gz")
    checksum = output / (name + ".tar.gz.sha256")
    for target in (directory, archive, checksum):
        if target.exists() or target.is_symlink():
            raise PackageError("版本输出已存在，请使用新的版本号：%s" % target)
    sources = list(RELEASE_FILES) + tracked_files(repo, "backend/resources")
    if len(sources) == len(RELEASE_FILES):
        raise PackageError("缺少已跟踪的运行资源。")
    tracked = set(tracked_files(repo, "."))
    for source in sources:
        if source not in tracked:
            raise PackageError("发布文件尚未提交：%s" % source)
        safe_source(repo, source)
    if go_image is not None and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9./:@_-]*", go_image):
        raise PackageError("无效的 Go 构建镜像。")
    for mount in (repo, output):
        if ":" in str(mount) or "\n" in str(mount):
            raise PackageError("Docker 挂载路径不能包含冒号或换行符。")
    output.mkdir(parents=True, exist_ok=True)
    lock = output / ("." + name + ".lock")
    try:
        descriptor = os.open(str(lock), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError as exc:
        raise PackageError("该版本正在打包；如上次进程被强制终止，请先确认再移除 %s" % lock) from exc
    os.close(descriptor)
    published = []
    published_directory = False
    try:
        with tempfile.TemporaryDirectory(prefix="." + name + "-", dir=output) as scratch:
            stage = Path(scratch)
            source_tree = stage / "source"
            source_snapshot(repo, commit, source_tree)
            migration_files = [path.relative_to(source_tree).as_posix()
                               for path in sorted((source_tree / "backend/migrations").glob("*.sql"))]
            catalog = migration_catalog(source_tree, migration_files)
            module = (source_tree / "backend/go.mod").read_text(encoding="utf-8")
            match = re.search(r"^go ([0-9]+\.[0-9]+(?:\.[0-9]+)?)\s*$", module, re.MULTILINE)
            if not match:
                raise PackageError("backend/go.mod 缺少有效的 Go 版本。")
            go_version = match.group(1)
            image = go_image or "golang:" + go_version
            bundle = stage / "sub2api-car"
            bundle.mkdir()
            report("[1/5] 验证更新脚本并安装前端锁定依赖")
            runner([sys.executable, "-B", "-m", "unittest", "discover", "-s", "deploy/tests", "-p", "test_*car.py"], source_tree)
            runner(["pnpm", "install", "--frozen-lockfile"], source_tree / "frontend")
            report("[2/5] 构建前端（含类型检查和语言完整性检查）")
            if test_app:
                runner(["pnpm", "run", "lint:check"], source_tree / "frontend")
                runner(["pnpm", "run", "test:run"], source_tree / "frontend")
            runner(["pnpm", "run", "build"], source_tree / "frontend")
            builder = [
                "docker", "run", "--rm",
                "-e", "GOTOOLCHAIN=go" + go_version,
                "-e", "GOMAXPROCS=2", "-e", "GOMEMLIMIT=3GiB", "-e", "GOGC=30",
                "-v", str(source_tree / "backend") + ":/workspace",
                "-v", "sub2api-car-go-mod:/go/pkg/mod",
                "-v", "sub2api-car-go-build:/root/.cache/go-build",
                "-w", "/workspace",
            ]
            if test_app:
                report("执行后端单元测试；依赖外部数据库的检查仍需独立 CI 环境")
                runner(builder + [image, "go", "test", "-tags=unit", "./..."], repo)
            report("[3/5] 编译 Linux amd64 程序并验证版本")
            stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            flags = "-s -w -X main.Version=%s -X main.Commit=%s -X main.Date=%s -X main.BuildType=release" % (version, commit, stamp)
            runner(builder + [
                "-e", "CGO_ENABLED=0", "-e", "GOOS=linux", "-e", "GOARCH=amd64",
                "-v", str(bundle) + ":/output", image,
                "go", "build", "-buildvcs=false", "-trimpath", "-tags=embed",
                "-ldflags", flags, "-o", "/output/sub2api", "./cmd/server",
            ], repo)
            binary = bundle / "sub2api"
            with binary.open("rb") as stream:
                header = stream.read(20)
            if header[:7] != b"\x7fELF\x02\x01\x01" or header[18:20] != b"\x3e\x00" or not binary.stat().st_mode & 0o111:
                raise PackageError("编译结果不是可执行的 Linux amd64 程序。")
            observed = runner([
                "docker", "run", "--rm", "--network", "none", "--read-only",
                "-v", str(bundle) + ":/release:ro", image,
                "/release/sub2api", "--version",
            ], repo, capture=True)
            if not re.search(r"Sub2API " + re.escape(version) + r" \(commit: " + re.escape(commit) + r",", observed):
                raise PackageError("编译程序的版本或提交号不匹配，拒绝发布。")
            report("[4/5] 生成文件校验清单与数据库迁移清单")
            for source in sources:
                destination = bundle / source
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(safe_source(source_tree, source), destination)
                destination.chmod(0o755 if source.endswith(".sh") else 0o644)
            (bundle / "BUILD_VERSION").write_text(version + "\n", encoding="utf-8")
            (bundle / "BUILD_COMMIT").write_text(commit + "\n", encoding="utf-8")
            (bundle / "MIGRATIONS.json").write_text(json.dumps(catalog, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            manifest = "".join(digest_file(path) + "  " + path.relative_to(bundle).as_posix() + "\n"
                               for path in sorted(bundle.rglob("*")) if path.is_file())
            (bundle / "SHA256SUMS").write_text(manifest, encoding="utf-8")
            staged_archive = stage / archive.name
            with tarfile.open(staged_archive, "w:gz", format=tarfile.PAX_FORMAT) as tar:
                tar.add(bundle, arcname="sub2api-car")
            digest = digest_file(staged_archive)
            report("[5/5] 使用服务器校验器验证完整部署包")
            runner([sys.executable, "-B", str(source_tree / "deploy/upgrade_car.py"),
                    str(staged_archive), "--sha256", digest, "--check-only"], source_tree)
            if clean_commit(repo) != commit:
                raise PackageError("构建期间 Git 提交发生变化，请重新构建。")
            staged_checksum = stage / checksum.name
            staged_checksum.write_text(digest + "  " + archive.name + "\n", encoding="utf-8")
            # Hard links publish files without replacing an existing artifact.
            os.link(staged_archive, archive)
            published.append(archive)
            os.link(staged_checksum, checksum)
            published.append(checksum)
            if directory.exists() or directory.is_symlink():
                raise PackageError("输出目录在构建期间已被创建，拒绝覆盖。")
            bundle.rename(directory)
            published_directory = True
        return Release(directory, archive, checksum, digest, commit)
    except BaseException:
        if published_directory:
            shutil.rmtree(directory)
        for path in published:
            path.unlink()
        raise
    finally:
        lock.unlink()


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version", help="新版本号，例如 0.2.17-car.6")
    parser.add_argument("--output-dir", type=Path, help="输出目录，默认仓库 output/")
    parser.add_argument("--go-image", help="已有 Go 构建镜像；实际工具链仍由 go.mod 指定")
    parser.add_argument("--test-app", action="store_true", help="额外执行前端 lint/测试与后端单元测试")
    args = parser.parse_args(argv)
    try:
        release = package_release(Path(__file__).resolve().parents[1], args.version,
                                  args.output_dir, args.go_image, args.test_app)
    except (PackageError, OSError, UnicodeError, tarfile.TarError) as exc:
        print("打包失败：%s" % exc, file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("打包已中断，没有发布不完整部署包。", file=sys.stderr)
        return 130
    print("\nPACKAGE_OK\n版本：%s\n提交：%s\n部署包：%s\n校验文件：%s\nSHA256：%s" % (
        args.version, release.commit, release.archive, release.checksum, release.sha256))
    print("上传部署包和校验文件到服务器 /home/ubuntu 后执行：\n"
          "sudo python3 /opt/sub2api-car/deploy/upgrade_car.py /home/ubuntu/" + release.archive.name)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
