# Carpool Subscription Reset Monitor

The server checks the shared OpenAI/Codex account every ten minutes, at minutes
00, 10, 20, 30, 40 and 50 in Asia/Shanghai. It also observes once on startup.
No browser session or external cron job is required.

## Activation

Exactly one active OpenAI subscription group must have `carpool_members` set to
2-5. That group must bind exactly one non-deleted, active, non-shadow OpenAI
OAuth account with a `chatgpt_account_id`. Auto-paused expired accounts are not
eligible. An ambiguous or unavailable source disables detection and cancels
pending events after transactional revalidation.

The first valid observation of a new source only establishes its baseline.
The baseline persists across server restarts. Source changes invalidate older
observations and pending events; the next check cancels old work and establishes
a new baseline. Database triggers append relevant identity, configuration and
binding changes, including changes reverted between polls. They do not lock the
monitor singleton inside normal business transactions. Ordinary access-token
refreshes and quota-limit updates are ignored.

## Reset Evidence

The monitor queries usage read-only; it never consumes reset credits or calls a
model. Only fresh, explicitly populated seven-day windows are accepted.

- A seven-day reset timestamp must advance by more than 60 seconds and imply a
  new window start within the observation interval. This also catches a reset
  followed by new usage, such as 20% to 0.5%, or a natural weekly rollover.
- If the window timestamp has not advanced, a successful upstream reset receipt
  recorded by this server and a clear usage drop are both required. The new
  percentage must be lower and at most `max(1%, previous percentage * 10%)`.
- A percentage drop without corroborating evidence does not trigger a reset.
  An external reset that leaves the timestamp unchanged cannot be confirmed by
  this monitor alone; the existing manual reset-all action remains available.
- Missing values, errors, old/out-of-order snapshots and duplicate evidence do
  not trigger resets. Delayed window timestamps and late receipts are deduped.

Receipts contain hashed upstream account and request identifiers, not tokens or
credit IDs. A receipt-storage failure is logged but does not turn a successful
upstream consume into an error or trigger another consume.

## Subscription Effects

Each event captures all globally active, unexpired, non-deleted subscriptions,
across groups and pages. Execution rechecks identity, status, expiry and the
subscription term start. Revoked, expired, deleted, reassigned or renewed terms
are skipped. New subscriptions created after capture are not added retroactively.
Extending an existing active term without replacing its start keeps it eligible.

Application is a single transaction with the same field semantics as resetting
all three quota windows manually:

- Set daily, weekly and monthly used amounts to zero.
- Set the daily window to the current day's start in the configured server
  timezone; set weekly and monthly windows to the execution time.
- Preserve limits, expiry, status, balances and historical usage logs.

The carpool weekly-limit adjustment runs every 30 minutes, independently of usage resets.

## Recovery And Concurrency

An immutable target snapshot and event ledger prevent repeated resets after a
restart or lost response. Database failure rolls back the entire batch. Pending
events retry on later checks for up to 30 minutes; newer confirmed events
supersede older pending events. Completed events are never applied again.

Cache invalidations are stored durably and retried separately. A Redis or cache
failure never causes quota resets to be replayed.

Short, non-waiting shared table locks on source configuration protect against
concurrent account/group/binding changes, including restored or newly bound
accounts. Busy source writers make the transaction fail closed and retry later.
Transactions have a ten-second overall timeout. Expiry is rechecked against the database clock after
target rows are locked. No upstream call occurs inside a reset transaction.

## Deployment And Diagnosis

Deploy a build containing migration `238_custom_carpool_reset_monitor.sql` and
the earlier `237_custom_group_carpool.sql`. Normal server startup applies these
migrations. Back up the database before deploying. The reset-monitor migration
adds its own tables and source-change triggers; it does not reset subscription
data itself.

Search server logs for `service.carpool_reset` or `carpool_reset_receipt_failed`.
Event records are in `carpool_subscription_reset_events`; immutable targets and
cache acknowledgements are in `carpool_subscription_reset_targets`. Source
generations use the committed row count of the append-only
`carpool_reset_source_changes` table, including out-of-order commits. Do not erase
these tables to retry a completed event, because they contain its deduplication
history. Disabling carpool configuration stops future automatic detection.

Automated tests use mock upstream HTTP and an explicitly isolated
`CARPOOL_TEST_DATABASE_URL`. Never point this test variable at a business database.
