# Carpool update automation implementation plan

> Execute in this isolated checkout using subagent-driven-development. The user selected local packaging plus one server upgrade command.

**Goal:** Replace the repeated manual packaging and deployment commands with two reproducible Python 3 scripts while retaining the existing Compose installation and operator-controlled update timing.

**Architecture:** `deploy/package_car.py` produces the existing `sub2api-car/` archive, adds the standalone `deploy/upgrade_car.py` and a SQL migration checksum catalog, and emits the outer checksum. `upgrade_car.py` verifies the release, prepares an image, backs up the live installation, and replaces only the application container. Existing `.env`, Compose/Caddy configuration, data directories and database/cache/proxy containers remain under the operator's control.

**Tech stack:** Python 3 standard library, Git, pnpm, Docker and Docker Compose. No new account, registry, token, SSH access or scheduled deployment.

## Release contract

- Required release files remain `sub2api`, `BUILD_VERSION`, `BUILD_COMMIT`, `Dockerfile.goreleaser`, `.dockerignore`, runtime resources and existing deployment files.
- Add `deploy/upgrade_car.py` and `MIGRATIONS.json` with shape `{"format_version": 1, "files": {"filename.sql": "sha256-of-trimmed-SQL"}}`. The catalog matches the application's `strings.TrimSpace` checksum convention. No secrets or mutable data enter the archive.
- `SHA256SUMS` lists every regular release file except itself. The adjacent `<archive>.sha256` contains the archive hash and basename.
- Version names and commit IDs are validated before use. Existing output releases are never silently overwritten. Git must be clean before packaging.

## Tasks and checks

The list below records the implementation sequence; release acceptance is verified by the actual test and packaging runs.

- Implement and test `deploy/package_car.py` and `deploy/tests/test_package_car.py`. Build from an immutable commit snapshot, with frontend before the embedded Linux amd64 Go binary; derive the toolchain and migration catalog from that same snapshot, reuse Docker module/build caches, and record version/commit/build time. Copy an explicit file set and tracked runtime resources, generate both manifests, verify the result, and print the server command. Unit tests use temporary Git repositories and fake subprocesses; a final real packaging smoke test exercises the complete path.
- Implement and test the standalone `deploy/upgrade_car.py` and `deploy/tests/test_upgrade_car.py`. Accept an archive plus `--sha256` or its adjacent checksum, with `--check-only` for read-only validation. Reject traversal, links, devices, duplicate paths, unlisted files, altered checksums, secret/data paths and malformed metadata. Copy the verified archive into a private staging directory before extraction. Serialize upgrades with a file lock.
- Verify the existing installation and capture its running image, version and schema-migration records. Build and verify the new image before touching the running installation. Create a private PostgreSQL dump (validate with `pg_restore --list`) plus configuration, application-data and managed-program backups. Do not print secrets or automatically dump application logs.
- Update managed program/resource/helper files atomically and change only `SUB2API_CAR_VERSION` in `.env`, preserving other values and permissions. Retain installed Compose and Caddy configuration. Run `compose config --quiet`, then `up -d --no-deps --no-build --pull never --timeout 30 --wait` for `sub2api` only; verify the running version and health.
- On failure before replacement, leave the running service intact. Automatic application rollback is permitted only when the new catalog proves there are no pending/changed SQL migrations and the post-failure migration snapshot is unchanged; otherwise stop the failed application and leave backups and a recovery report for the operator. Never restore the database automatically. Tests inject checksum, backup, startup, version and migration failures and prove data/config preservation and app-only operations.
- Add script tests to CI and update `deploy/CARPOOL_DEPLOYMENT.md` with one-time standalone helper installation, local packaging, archive plus checksum upload, one-command upgrade, maintenance-window behavior and failure recovery. Run unit tests, CLI help/check-only checks, a real package build and independent review. Merge and push only after verification. Do not run the new updater against production as a test.

## Commands to document

```bash
python3 deploy/package_car.py 0.2.17-car.6
sudo python3 /opt/sub2api-car/deploy/upgrade_car.py /home/ubuntu/sub2api-car-0.2.17-car.6-linux-amd64.tar.gz
python3 -m unittest discover -s deploy/tests -p 'test_*car.py'
```

Code changes and upstream merges still require their appropriate tests before release. Packaging must not imply that unrun integration tests passed. Script implementation tests and frontend build checks run automatically; optional application tests are explicit.
