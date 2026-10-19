# Carpool Reset Voting

## Activation

- Deploy backend and frontend from the same revision. Back up PostgreSQL first.
- Startup migrations add `241_custom_quota_reset_operations.sql` and
  `242_custom_carpool_reset_voting.sql`, after the existing carpool migrations.
- Keep the current single active OpenAI carpool group, bound to one OAuth parent
  account. Set `carpool_members` to the exact number of distinct active users with
  active, unexpired subscriptions (2 through 5).
- Disable automatic reset-card use in the existing account settings when user
  consent is required. This feature does not change those switches.
- The application worker scans durable proposals on startup and every 15 seconds.
  Browser status polling is database-only and runs every 10 seconds.

## Rules

- A request lasts 24 hours; the initiator contributes one approval.
- Every configured member must approve. Duplicate clicks do not add votes.
- Subscription lifecycle, user eligibility, group configuration and account
  binding changes invalidate unexecuted votes, including changed-back settings.
  Extending the same still-active subscription term does not invalidate votes.
- An acknowledged manual reset or a detected natural window rollover invalidates
  older unexecuted votes. Initial consumption also checks fresh upstream usage.
- One account may have only one unresolved consume operation. Admin requests
  bypass voting but cannot bypass this guard.
- A vote persists its selected card and upstream request identity before sending.
  Unknown results retry that identity only. A denial on a replay is not proof that
  the original request failed, so it cannot authorize another card.
- Lost creation acknowledgements can be reconciled with a database-only GET
  using the original `request_id`; `request_resolved` is true only for a durable
  request mapping belonging to the authenticated user.
- Confirmed consumption creates one durable global subscription reset event.
  Eligible targets' daily, weekly and monthly used amounts are cleared; limits,
  balances, subscription expiry and historical usage logs are unchanged.
- The immutable target snapshot is captured immediately before dispatch.
  Revoked, expired, deleted or replaced targets are checked again and skipped.
  Subscriptions added after dispatch are not retroactively reset.
- The reset monitor recognizes the same event, including delayed database
  recovery, rather than clearing quota again. Cache/account-state recovery is
  retried without additional card consumption.

## Uncertain Outcomes

`awaiting_confirmation` intentionally continues to reserve the account even
after the original 24-hour voting deadline. Do not delete operation/proposal
records or downgrade to a version without the coordinator to unblock it: that
would discard the duplicate-consumption protection.

If the upstream cannot confirm the original request, or the account/membership
changed while its result was uncertain, automatic spending remains blocked.
An operator must establish the original outcome before repairing persisted
state. This release does not add a force-unlock or a second-card fallback.
When an affirmative local receipt already exists for the exact original
identity/request, recovery uses that receipt without contacting the consume
endpoint, even if membership or account configuration subsequently changed.
The background scan also reconciles these receipts when no vote is active or
automatic card use is disabled. It takes the same account lock as consumption
and leaves an in-flight consume untouched. A different request may repair the
old operation, but receives a conflict instead of inheriting its success; a
separate retry is required, and unfinished vote application remains protected.

The application cannot make external upstream account changes atomic with local
database transactions. Keep the single shared-account configuration stable while
an operation is in progress.

## Verification

Repository tests use a disposable PostgreSQL database via
`CARPOOL_TEST_DATABASE_URL`, with an isolated schema per test. Never point this
variable at a business database. Focused tests cover duplicate requests,
concurrent approvals, changed-back membership, expiry, manual/natural resets,
uncertain replay, restart recovery, and reset-monitor deduplication.
