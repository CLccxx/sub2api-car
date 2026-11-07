# Carpool Reset Voting

## Activation

- Deploy backend and frontend from the same revision. Back up PostgreSQL first.
- Startup migrations add `241_custom_quota_reset_operations.sql` and
  `242_custom_carpool_reset_voting.sql`, followed by
  `249_custom_carpool_account_scope.sql` for account-scoped monitoring and voting.
- Bind each subscription group to one eligible OpenAI OAuth parent account. An
  account can serve ordinary subscriptions and several quota-tier groups. Its electorate is all
  distinct active users with started, active, unexpired subscriptions across its
  bound subscription groups, including ordinary subscription groups. The
  `carpool_members` value controls the quota divisor independently of electorate
  size. One voter and electorates above five are supported.
- Disable automatic reset-card use in the existing account settings when user
  consent is required. This feature does not change those switches.
- The application worker scans durable proposals on startup and every 15 seconds.
  Browser status polling is database-only and runs every 10 seconds.

## Rules

- A request lasts 24 hours; the initiator contributes one approval.
- Every eligible account subscription member must approve. A user in multiple
  groups has one vote. Members can view and approve the same account proposal
  from their own subscription group. The initiator's approval completes a
  one-person electorate. Duplicate clicks do not add votes.
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
- Confirmed consumption creates one durable account-scoped subscription reset
  event. It clears eligible targets' daily, weekly and monthly used amounts and
  retains limits, balances, subscription expiry and historical usage logs.
- The immutable target snapshot is captured immediately before dispatch.
  Revoked, expired, deleted, rebound or replaced targets are checked again and skipped.
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

The account-scope migration invalidates old proposals that have not been sent,
because their approvals used the old electorate. Dispatched historical proposals
retain their durable outcomes for recovery; an uncertain historical request is
not replayed under the new voting rules.

The application cannot make external upstream account changes atomic with local
database transactions. Keep the affected account and its group bindings stable
while an operation is in progress.

## Verification

Repository tests use a disposable PostgreSQL database via
`CARPOOL_TEST_DATABASE_URL`, with an isolated schema per test. Never point this
variable at a business database. Focused tests cover duplicate requests,
concurrent approvals, changed-back membership, expiry, manual/natural resets,
uncertain replay, restart recovery, and reset-monitor deduplication.
