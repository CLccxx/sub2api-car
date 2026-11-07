# Subscription estimate and credit consumption split

Date: 2026-11-07. Base: v0.2.19-car.2 / 9fc2dc2fd.

## Confirmed behavior

- Only the OpenAI OAuth parent account's seven-day utilization reaching 100%
  freezes the subscription estimate. Five-hour exhaustion does not trigger it.
- Before exhaustion, retain the current dynamic account-cost estimate. At the
  first valid observed exhaustion, persist the subscription baseline once for
  the account/cycle. Point spending must not increase that baseline afterward.
- Credits-used cost is the nonnegative post-baseline account cost difference;
  it is a dollar cost, separate from the upstream remaining credits balance.
- A first observation already at 100%, without a tracked prior baseline, uses
  the user-selected $1600 subscription baseline for that cycle. No manual editor.
  This is an initial-unknown-cycle fallback, not a permanent default for later
  known reset cycles. A valid empty cost aggregate can still use that baseline;
  database failures and invalid cost rows remain unavailable.
- Natural seven-day resets and confirmed reset-card resets start a new cycle.
  Old delayed observations must not resurrect a prior cycle or refreeze a new
  cycle from stale pre-reset utilization. Reset cutoffs exclude prior-cycle cost.
- One account's state is shared by all its groups, and survives restart. Other
  accounts, re-bound identities, API-key accounts and shadow accounts stay isolated.
- Carpool caps consume only the subscription estimate, with the existing
  ceil-to-$10 / members / floor-to-cents / +$100 calculation. Point spending does
  not raise those caps. At the $1600 legacy baseline: 4-person $500, 5-person $420.
- Admin account usage and user subscription usage show subscription estimate
  plus credits-used cost consistently. Preserve existing remaining-points display.

## Implementation contract

The seven-day projections on both administrator UsageProgress and user
SharedUsageWindow expose `estimated_total_cost` as subscription-only estimate,
`credits_used_cost` as nullable number, `estimate_frozen` as boolean, and
`estimate_source` as `dynamic`, `observed`, or `legacy_default` when available.
The backend owns this calculation; the admin frontend must stop independently
dividing the combined window cost by utilization.

Use durable per-account/cycle observation state and an additive migration (250).
Capture successful traffic observations after accounting where possible, outside
the ordinary snapshot throttle, with bounded background sampling for unattended
accounts. Headerless traffic and periodic polling share a per-account minimum
one-minute sampling interval, including failures. Do not depend on opening either
UI, and never consume credits to sample.
Use atomic first-write/transaction semantics to prevent concurrent overwrite.
Preserve receipt/event deduplication and handle reset proof before accepting a new
cycle. Missing/stale/invalid data must not freeze a bogus amount or restore the old
combined-cost quota calculation.

Register pending accounting durably before queue submission. Preserve the first
full observation's cutoff while earlier completed requests settle, then freeze
only the costs at or before that cutoff. Later credit rows must not affect the
baseline even if they reach storage first. Pending lower-utilization evidence
must prevent the legacy default from replacing a known observed transition.
An unresolved accounting gap leaves the estimate unavailable; a confirmed new
cycle or identity excludes the preceding cycle's gaps.

## Tasks and ownership

1. Backend implementation agent: all backend tracker state, migration, repository,
   observation/accounting/reset hooks, admin and shared projections, quota source,
   dependency wiring and backend tests. Send the API contract early; keep changes
   separate from frontend files. Add regressions before fixes.
2. Parent: frontend shared/admin displays, shared type updates, i18n, UI regression
   tests and visual validation; operational documentation and integration checks.
3. Independent spec review, fixes, then quality review, final tests and local commit.
   Publishing/deployment is a separate follow-up unless requested in this task.

## Verification

- [x] Dynamic estimates, first freeze, credit growth and legacy $1600 fallback.
- [x] Restart, concurrency, stale/out-of-order observations and account isolation.
- [x] Deferred pre-cutoff accounting, late lower-utilization evidence, credit rows
      arriving first, restart persistence, and unresolved-gap reset isolation.
- [x] Natural/card reset and historical-cost exclusion, including unchanged reset timestamp.
- [x] Both API projections and quota calculations consume the same baseline.
- [x] PostgreSQL migration/preservation plus service/repository tests and lint/build.
- [x] Frontend tests/typecheck/lint/build and rendered admin/user views.
- [x] Independent spec and quality review, then commit verified changes.

### Verification record

- Frontend: 385 test files / 3,185 tests passed; typecheck, lint and production
  build passed. Rendered administrator and subscriber views were checked in
  Chinese/English, light/dark themes, desktop and narrow layouts.
- Backend: full unit and integration suites were run, with the affected handler,
  repository and service packages rerun successfully after attribution and
  migration-fixture corrections. The final subscription-cost suite passed after
  adding the sampling interval, expiry and bounded-history regressions.
- Real PostgreSQL checks cover official-to-car and existing-car upgrades,
  preserved data, atomic raw insert/dedup, concurrent freeze/registration,
  delayed accounting across restart, and reset/identity isolation.
- `golangci-lint run ./...`: zero issues. Embedded production build and
  `git diff --check` passed. Independent specification and code-quality reviews
  approved the final implementation.
