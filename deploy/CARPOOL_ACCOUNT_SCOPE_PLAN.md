# Account-scoped subscription resets and quota tiers

Date: 2026-11-07. Base: v0.2.19-car.1 / 76cadaa46.

## User-approved behavior

- Standard/balance groups continue balance billing. An upstream account reset
  clears used subscription quota only in subscription groups bound to that
  account, including ordinary subscription groups with carpool disabled.
- One upstream account can serve multiple carpool quota tiers (e.g. 4 and 5).
  Each tier uses the whole account estimate and its configured divisor.
- Weekly cap = floor(ceil(estimate / 10) * 10 * 100 / members) / 100 + 100.
  Remove the 200 floor. Retain invalid-estimate guards and half-hour schedule.
  Use the existing 1600 bootstrap estimate with the same new formula when a
  group has no positive cap. Preserve an existing positive cap until sync.
- Voting requires unanimous approval from distinct active users with started,
  active, unexpired subscriptions in the account's bound subscription groups.
  A user across groups votes once; configured tier size is not electorate size.
  User explicitly chose this option in chat. One-person electorate completes
  on its initial approval; electorates above five are supported.
- Separate upstream accounts are monitored independently. Ambiguous group
  bindings retain fail-closed behavior for account selection.
- Follow-up clarification: every eligible OpenAI OAuth account bound to a
  subscription group is monitored, even if no carpool quota switch is enabled.
  API-key accounts and balance-only groups do not participate in monitoring.

## Design and implementation

Use account-level source resolution and persisted per-account observation
baselines while retaining durable event/receipt deduplication and lock ordering.
Use a new forward migration; leave published SQL immutable. Retain compatible
single-account baseline/history and handle old queued proposals safely.
Snapshot only account-bound subscription targets at event creation or immediately
before vote dispatch. Recheck current binding, subscription group type and term
at application, including old pending events with global snapshots. New targets
must never be added retroactively. Confirmed upstream consumption must never be
repeated to repair local persistence. Clear daily/weekly/monthly usage exactly
as today while preserving balance, cap, expiry and history.

Task ownership:

1. Quota worker owns group_carpool.go, quota tests, frontend carpoolSettings.ts
   and its component/helper tests. Add regression cases before formula change.
2. Reset worker owns backend carpool reset monitor/vote ports/services/repos,
   their tests and the new migration. Test two accounts, shared-account tiers,
   normal subscription vs balance groups, rebinds, legacy pending events,
   replay/rollback, electorate deduplication, and cross-group authorization.
3. Parent owns user-facing translations/docs, integration-fixture updates
   outside reset tests, review and end-to-end validation.

## Verification and delivery

- [x] Quota regression red/green, initialization parity and examples.
- [x] PostgreSQL monitor/vote regression red/green with account scope.
- [x] Existing idempotency, concurrency, receipt recovery and voting tests.
- [x] Migration upgrade/preservation coverage including previous version state.
- [x] Frontend lint/typecheck/tests/build; backend unit/integration/lint/build.
- [x] Independent review and required fixes; verified work ready for commit.

The live server currently runs car.1. This task changes code and validates the
next version; production rollout is a subsequent step with a new release.

Verification: quota backend regressions failed against the previous formula
then passed with the new calculation; frontend quota tests passed 21 cases.
Frontend full suite passes 3,179 tests across 385 files, along with lint,
typecheck and production build. Account-wide vote regressions were observed
failing against the previous implementation before reset changes began.

Final backend checks passed: service/handler/server unit suites; all carpool
service regressions after the reset changes; full repository unit and PostgreSQL/
Redis integration suites; golangci-lint (zero issues); and the server build with
embedded frontend assets. Upgrade tests cover official 0.2.18 and car.1, preserving
durable user/account/key/subscription data, historical migration checksums, JWT
validation and refresh-token rotation.

Review regressions reproduced then resolved shared balance-group interference
between otherwise independent subscription accounts, and executable vote
starvation behind unfinished ballots. Legacy baseline cutover, legacy uncertain
dispatch, changed-back bindings, ordinary subscription membership, and independent
account read failures are covered. No published migration was edited.
