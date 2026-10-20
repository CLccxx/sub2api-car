# Carpool version-panel updater implementation plan

> Use subagent-driven-development with explicit file ownership, TDD and separate spec/code reviews. Keep implementation in the isolated `codex/carpool-web-updates` branch until verified.

**Goal:** Refresh, prepare and activate the latest release from our GitHub repository using the existing administrator version panel.

**Architecture:** Authenticated Go handlers proxy the fixed v1 Unix-socket protocol to a host Python updater. The host service prepares a pinned full release and invokes the existing migration-aware upgrade transaction on activation. A dedicated tagged-release workflow publishes complete custom bundles as full/latest releases.

**Tech stack:** Go, Vue/TypeScript, Python 3.9+ standard library, Docker Compose, systemd, GitHub Releases/Actions.

## Task 1: Host release client and operation service

Ownership: worker owns `deploy/car_release.py`, `deploy/car_update_service.py`, and their `deploy/tests/test_*car.py` files. Keep release HTTP validation separate from operation state/server code. Parent owns application source, publishing workflow and documentation.

- [x] Write failing tests for exact repository/asset matching, bounded downloads, checksum and redirect failures, custom version ordering, concurrent preparation rejection, and persistent ready state.
- [x] Implement the v1 protocol in the design with dependency-injected HTTP/downloader/runner boundaries. Use the existing `upgrade_car` verifier/transaction; never implement a second database rollback policy.
- [x] Verify preparation creates no backup or live-file changes, while activation uses the same pinned artifact and preserves migration safety. Test initial/repeated/invalid activation and interrupted recovery states.
- [x] Run `python3 -B -m unittest discover -s deploy/tests -p 'test_*car.py'`, then spec and code review before merging the worker's changes.

## Task 2: Managed updater integration in Go

Ownership: parent; files `backend/internal/service/update_service.go`, a focused `car_update_client.go`, relevant tests, `backend/internal/handler/admin/system_handler.go`, route registration and configuration/provider wiring.

- [x] First reproduce current bugs: `compareVersions("0.2.17-car.6", "0.2.17-car.7")` must be negative; car.10 must sort after car.9. A cache entry for the upstream repository must not be reused for the fork.
- [x] Implement repository-aware release metadata and strict custom release validation, keeping legacy version behavior outside custom builds.
- [x] Implement a context-bounded Unix HTTP client for `/v1/status`, `/v1/prepare`, `/v1/activate`. Limit JSON response size and reject malformed protocol state. It accepts only a configured absolute socket path.
- [x] Add managed status/prepare/activate methods without silently selecting legacy binary replacement for custom builds. Existing admin authentication, idempotency and operation locks remain in use.
- [x] Add `GET /api/v1/admin/system/update-status`; update and restart return managed operation metadata. Test absent daemon, invalid response, retries and no legacy fallback.
- [x] Run focused service/handler unit tests and Go static checks.

## Task 3: Version panel state and reconnect behavior

Ownership: parent; files `frontend/src/api/admin/system.ts`, `frontend/src/stores/app.ts`, `frontend/src/components/common/VersionBadge.vue`, a focused composable if needed, tests and locale files.

- [x] Write failing UI tests for preparation without restart, restoring ready state after reload, delayed activation completion, connection loss, verified rollback and explicit failure.
- [x] Extend API types with managed status. Poll only while the panel/operation needs it; clean up timers and avoid concurrent polling calls.
- [x] Preserve the two actions Update and Restart now. Show the target version and bounded progress/error text. Reload only after the managed operation succeeds and the running version matches its target.
- [x] Use the configured repository for release links. Hide the legacy downgrade commands in managed mode. Show update-check warnings instead of claiming latest when lookup failed.
- [x] Run changed frontend tests, lint, locale completeness, typecheck and build. The immutable release build will repeat this from the tagged source snapshot.

## Task 4: Bootstrap and complete-bundle integration

Ownership: worker for `deploy/install_car_web_update.py`, `deploy/sub2api-car-updater.service`, `deploy/docker-compose.car-updater.yml`, updater tests and narrowly coordinated changes to `deploy/upgrade_car.py`; parent owns `deploy/package_car.py` allowlist and its tests.

- [x] Write failing tests for the one-time installation, preserved existing config/secrets, socket ownership, explicit Compose override and failure before switching.
- [x] Add only known web-updater files to the bundle allowlist. Include the installed override in all helper Compose invocations, retain it during ordinary updates, and do not replace operator configuration automatically.
- [x] Implement a root-only bootstrap that works with the current car.6 installation. It verifies the complete new release, prepares the socket directory/override/service, upgrades the app once and enables the host service. It never asks for or prints private application credentials.
- [x] Verify idempotent reinstallation and a missing/failed daemon without affecting the current application.

## Task 5: GitHub publication and operator documentation

Ownership: parent; `.github/workflows/car-release.yml`, exclusions in the inherited release workflow, a small tested publishing helper if needed, and `deploy/CARPOOL_DEPLOYMENT.md`.

- [x] Build only custom version tags and bind artifacts to their exact commit. Run deployment-tool and appropriate application checks before publication.
- [x] Publish a draft first, attach the full archive, checksum and standalone bootstrap, then mark the complete release full/latest. Reject an already published version instead of overwriting it.
- [x] Exclude custom tags from upstream release automation; preserve its behavior for noncustom tags.
- [x] Document the final one-time installation and subsequent browser flow. Explain that releases are deliberate published versions and that changing repository visibility exposes source/history.
- [x] Validate workflow structure and publishing logic without creating a public release during development.

## Task 6: Acceptance and delivery

- [x] Run all affected tests and independent spec/code review; fix important findings.
- [x] Exercise prepare/activate/reconnect and failure recovery on an isolated Compose stack with real PostgreSQL, preserving infrastructure container identities and data.
- [ ] Build and verify the real car.7 bootstrap release. Keep the healthy production car.6 instance unchanged while preparing delivery.
- [ ] Resolve GitHub authentication/visibility and release publishing with the user only after the change is concrete and reviewable; never reveal previously pasted credentials.
- [ ] Publish the verified custom release and guide the one-time server bootstrap. Confirm public health and the version panel's fork release source.

## Verification progress

- Application spec and quality reviews passed after deferred enqueue / multi-tab regressions were fixed.
- Host spec and quality reviews passed after deadline, image pin, recovery reload and full migration-ledger regressions were fixed.
- Publication spec and quality reviews passed after preserving noncustom manual dry runs and verifying the remote tag before both mutation stages.
- Real Linux Unix-socket test passed: app UID 1000 can communicate through a read-only socket-directory mount; UID 1001 is rejected. No Docker control socket was exposed.
- Actual version components were exercised in a local browser fixture: prepare, ready, refresh and activate states rendered correctly.
- Bootstrap spec, quality and final cross-component integration reviews passed after stopped-service signal, persistent boot enablement and older-systemd first-install regressions were fixed. All 152 deployment tests passed on Python 3.9 and 3.13, including an independent complete rerun. The isolated full Compose transaction passed same-version stale mount repair, prepare, activation, safe rollback and migration-gated stop checks while preserving infrastructure and data. Actual Ubuntu systemd/reboot acceptance remains part of server bootstrap; no production or GitHub publication changes have been made.
