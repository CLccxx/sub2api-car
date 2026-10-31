# Official 0.2.19 synchronization and migration preparation

Date: 2026-10-31

## Approved scope and design

The user requested synchronizing the existing car edition to official v0.2.19,
then preserving user data, settings, upstream authorizations, and valid browser
sessions during a later server migration. Their existing server is official
0.2.18, probably deployed with Docker. This task prepares and verifies the code;
production deployment awaits inspection of that server's actual configuration.

Merge the complete official v0.2.19 tag (commit dece26153) into the current car
edition (a31742553). This retains upstream history and fixes while preserving
carpool quotas, reset monitoring/voting, subscription operations, and managed
updates. Selective cherry-picking risks omissions; replacing the repository with
upstream would lose custom behavior. Resolve source conflicts, then regenerate
Ent/Wire outputs where necessary instead of hand-editing generated offsets.

Reuse the existing production PostgreSQL, Redis, app data, runtime configuration,
JWT/TOTP keys, and public origin at migration time. Do not initialize a new
installation, reset passwords, clear Redis, or rewrite historical migrations.
Additive schema changes must preserve existing business rows and credentials.
Normal token expiry/revocation still applies. Single-instance replacement may
interrupt requests; zero downtime and uninterrupted streams are not promised.

## Implementation plan

> Execute sequentially, with independent review after the integrated change.

- [x] Establish baseline: run the existing car deployment tests and focused
  backend/frontend checks; record any pre-existing or environmental failures.
- [x] Merge `git merge --no-commit --no-ff v0.2.19` on
  `codex/sync-official-0.2.19`. Preserve both carpool and upstream changes in
  overlapping group schemas, mappings, routes, frontend forms, and CI workflows.
  Use `go generate ./ent` and `go generate ./cmd/server` if generated code conflicts.
- [x] Validate the migration delta: official SQL files must match v0.2.19;
  existing custom SQL files must match a31742553. Rehearse official 0.2.18 schema
  upgrading to the merged schema in an isolated database with representative
  users, passwords, API keys, account credentials, balances, subscriptions,
  usage, and settings. Assert preservation and idempotency.
- [x] Verify browser token continuity with fixed keys and Redis-backed refresh
  tokens, and preserve upstream OAuth credential shapes. Keep ordinary groups
  outside carpool automation unless explicitly enabled.
- [x] Document the official-to-car Docker migration in
  `deploy/CARPOOL_OFFICIAL_MIGRATION.md` and link it from
  `deploy/CARPOOL_DEPLOYMENT.md`: discover effective config and actual mounts;
  prepare the image and isolated rehearsal; drain requests; preserve services,
  secrets and origin; switch only the app; validate health/data/session/API;
  retain rollback backups without promising automatic schema downgrade.
- [x] Run backend unit and integration tests, frontend lint/typecheck/critical
  tests and car-specific tests, frontend/backend builds, deployment tool tests,
  and a focused independent review. Resolve regressions before completion.
- [x] Commit the verified merge and preparation, record exact checks and limits,
  and report the branch and remaining server-specific migration steps. Do not
  publish a release, push, or modify production as part of this request.

## Verification record

- Baseline: 152 deployment-tool tests, 147 focused frontend tests and focused
  backend carpool/session/update suites passed.
- Official tag: `v0.2.19`, commit `dece26153621959ccd7635814f4b2d08304e681a`.
  The three conflicts were generated Ent files; Ent and Wire were regenerated
  with Go 1.27.3. Generator dependencies added checksums without changing go.mod.
- Corrected the source-build fallback VERSION to `0.2.19`; the official release
  tag still stores `0.2.18` and relies on release build flags. Car release flags
  continue to supply the full `0.2.19-car.N` version when publishing later.
- Integrity review: all 297 official 0.2.18 SQL files, 299 official 0.2.19 SQL
  files and 300 prior car SQL files remain byte-identical to their source refs.
- The PostgreSQL 18 migration rehearsal passed with whole-row comparisons for
  12 tables, unchanged historical checksums/timestamps, and a second startup.
  Its enhanced session check passed with persisted JWT reuse, valid existing
  access tokens and Redis-backed refresh rotation in the same session family.
- Frontend: lint, typecheck, 576 critical tests, all 3,165 tests across 384 files,
  and production build passed. Existing test fixture warnings and bundle-size
  warnings do not fail these checks.
- Backend: full integration suite passed with Docker available and CI mode
  enabled; full serial unit suite also passed. The first
  parallel unit run had its service compiler killed under Docker memory pressure;
  the serial rerun uses `-p 1`, `GOMAXPROCS=2` and `GOGC=50`.
- Final `golangci-lint v2.14.0 run --concurrency=2 --timeout=30m` passed with
  zero issues. The embedded-frontend Go build succeeded, and the executable
  reported `Sub2API 0.2.19`. This is a verification build, not a published release.
- Final deployment tools: all 152 tests passed. Docker security, gateway/simple
  mode environment, runtime-resource and Caddy streaming/cache checks passed.
- Independent spec and code-quality reviews found no remaining issues. Added an
  online runbook link for deployment bundles without changing the archive format.

No production changes, release publication, or remote push have been performed.
