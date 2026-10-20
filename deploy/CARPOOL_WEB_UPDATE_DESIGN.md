# Carpool updates from the version panel

## Requested behavior

The administrator keeps the existing version popover: refresh to discover the latest custom release, click Update to prepare it, then click Restart now to activate it. The source is `CLccxx/sub2api-car`. A published full GitHub Release is the update unit; a Git push alone is not a release. `0.2.17-car.10` is newer than `0.2.17-car.9`.

The current production installation is Ubuntu x86_64 with Docker Compose at `/opt/sub2api-car`, version `0.2.17-car.6`. PostgreSQL, Redis and Caddy stay running during application upgrades. Private `.env`, Compose/Caddy settings and persistent data remain intact. The existing `upgrade_car.py` already provides verified backups, exact image/version checks and migration-aware failure recovery.

## Chosen architecture

Use the version panel and authenticated administrator API as the control surface. A small host service listens only on a Unix socket and runs the existing deployment transaction. This updates the actual image tag and persistent installation, so recreating the container preserves the chosen release. The application container does not receive the Docker socket or host shell access.

The alternatives were modifying only the GitHub repository constant (insufficient: the current updater swaps only a binary inside the container) and persisting a self-updated binary in a volume (survives recreation but leaves the image/runtime resources and existing deployment helper inconsistent). The host service reuses the tested complete-bundle upgrade flow.

One final bootstrap installs the host service and an additional Compose file mounting only its Unix socket. After that bootstrap, ordinary updates require only the two existing browser buttons. Repository visibility is handled separately; the requested public repository allows unauthenticated downloads. Changing visibility or publishing releases is a distinct external action after the implementation is reviewable.

## Release contract

- Accept only `vX.Y.Z-car.N` full releases from the configured repository, with the exact archive `sub2api-car-X.Y.Z-car.N-linux-amd64.tar.gz` and adjacent `.sha256` asset.
- Preserve the complete bundle contract, manifest and migration catalog. Extend the explicit file allowlist for the web updater modules and installer; never admit runtime configuration or data into a release.
- Fetch metadata/assets over HTTPS from fixed GitHub API/release paths for that repository. Bound metadata and download sizes, reject redirects outside known GitHub asset hosts, and do not accept a client-provided command, URL, filesystem path or repository.
- The full archive hash and every internal file must pass the existing verifier. Check the built image's version and commit before activation.
- Publish custom tags through a dedicated GitHub Actions workflow. Publish as a full release with `make_latest=true`, after uploading and verifying all artifacts. Exclude custom tags from the inherited upstream publishing workflow to avoid duplicate publication or prerelease classification.

## Local updater protocol v1

Transport: HTTP over `/run/sub2api-car-updater/control.sock`, no TCP listener. The socket is accessible only to the application UID/group and root. Requests are JSON with a bounded body; unknown fields/actions are rejected. One operation is active at a time, and state survives application restarts.

`GET /v1/status` returns:

```json
{"protocol_version":1,"phase":"idle","operation_id":"","target_version":"","target_commit":"","message":"","error_code":"","backup_path":""}
```

Phases: `idle`, `preparing`, `ready`, `applying`, `succeeded`, `failed`, `rolled_back`, `manual_recovery_required`. The server may add a `step` field for fixed preparation/activation stages. Errors contain safe fixed descriptions, not child process output, credentials or stack traces.

`POST /v1/prepare` body is `{"operation_id":"sysop-...","version":"0.2.17-car.7"}`. The service independently resolves the latest release and verifies that the requested version still matches and is newer than the actual running application. It queues preparation and immediately returns status. Preparation downloads, verifies and builds the target image without changing live files or stopping the current app. Repeating the same operation is idempotent. A ready release is pinned to its checksum and commit.

`POST /v1/activate` body is `{"operation_id":"the-prepared-operation-id"}`. Only the matching ready operation may activate. It immediately returns `applying`, then invokes the existing upgrade transaction on the pinned archive in the background. The backup is created at activation time. The service continues running while the application container is recreated. Success, verified rollback or manual recovery requirements are persisted and returned by status.

State is private and root-owned. On daemon interruption, an unfinished operation is reported explicitly and is never silently replayed. A separate nonblocking process lock prevents two service instances. Updating the daemon's installed source must not interrupt an active transaction; reload after a completed transaction or through the managed service restart policy.

## Backend and frontend

- Custom builds query this fork's releases, compare the full custom version and scope cached metadata to the repository.
- Custom builds never fall back to the old in-container binary replacement when the host updater is unavailable.
- Add an authenticated update-status endpoint. Update and restart dispatch to prepare/activate in managed mode; retain existing behavior for unrelated legacy installations.
- The existing admin-only popover displays preparation progress, the prepared version, activation progress and safe failures. Ready state survives page reload by reading server state.
- Restart waits for the managed operation and target version, rather than treating an old application's `/health` response as completion. Temporary disconnection during activation is retried; a failed activation or verified rollback is displayed accurately.
- Show the configured release page. Managed mode does not expose the old upstream install commands or arbitrary version downgrade button; database-sensitive downgrades remain a separate recovery operation.
- Ordinary users retain the static version display.

## Installation and recovery

Keep the existing main Compose and Caddy files. Add a dedicated Compose override for the control socket and update-mode environment, and teach the existing helper to include that override when present. The bootstrap validates the existing installation and the trusted release before installing files, preserves private settings, configures the socket directory/systemd unit, and performs the first application upgrade once. The service starts after its modules are installed; the app tolerates temporary updater unavailability.

Do not mount `/var/run/docker.sock` into the app. The host daemon exposes only the fixed update protocol. Do not restore a database automatically. Existing schema preflight and post-failure ledger checks remain the condition for automatic application rollback.

## Acceptance checks

Version/source/cache regression tests; administrator routing and Unix transport tests; archive/download/redirect validation; idempotency/concurrency and durable state tests; prepare-without-mutation and activate-with-backup tests; existing 37 release tests; UI ready/reload/reconnect/failure tests; locale completeness/typecheck/build; publisher tag/draft/latest contract tests; an isolated Docker end-to-end exercise with real PostgreSQL proving that only the app changes and data persists. Test production neither by force-updating it nor by using previously exposed credentials.
