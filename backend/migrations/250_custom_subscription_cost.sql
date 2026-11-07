-- Additive, account-scoped state; group rebinding does not reset an allowance.
CREATE TABLE IF NOT EXISTS subscription_cost_account_epochs (
    account_id BIGINT PRIMARY KEY,
    revision BIGINT NOT NULL DEFAULT 1,
    identity_started_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS subscription_cost_states (
    account_id BIGINT PRIMARY KEY,
    identity_hash TEXT NOT NULL,
    identity_revision BIGINT NOT NULL DEFAULT 0,
    state JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE OR REPLACE FUNCTION subscription_cost_identity_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.platform,OLD.type,OLD.parent_account_id,btrim(COALESCE(OLD.credentials->>'chatgpt_account_id','')))
       IS DISTINCT FROM ROW(NEW.platform,NEW.type,NEW.parent_account_id,btrim(COALESCE(NEW.credentials->>'chatgpt_account_id',''))) THEN
        INSERT INTO subscription_cost_account_epochs(account_id,identity_started_at) VALUES(NEW.id,clock_timestamp())
        ON CONFLICT(account_id) DO UPDATE SET revision=subscription_cost_account_epochs.revision+1,identity_started_at=EXCLUDED.identity_started_at;
    END IF;
    RETURN NULL;
END; $$;
DROP TRIGGER IF EXISTS subscription_cost_identity_changed ON accounts;
CREATE TRIGGER subscription_cost_identity_changed AFTER UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION subscription_cost_identity_changed();

-- Preserve the reset monitor's historical second-resolution receipt semantics;
-- accounting needs the precise success time to exclude pre-reset costs.
ALTER TABLE carpool_account_reset_receipts ADD COLUMN IF NOT EXISTS cost_reset_at TIMESTAMPTZ;

-- Separate attribution from storage time. Billing may run after a reset or
-- credential switch; both the original request and completion must stay known.
ALTER TABLE IF EXISTS usage_logs ADD COLUMN IF NOT EXISTS account_cost_started_at TIMESTAMPTZ;
ALTER TABLE IF EXISTS usage_logs ADD COLUMN IF NOT EXISTS account_cost_observed_at TIMESTAMPTZ;
ALTER TABLE IF EXISTS usage_logs ADD COLUMN IF NOT EXISTS account_cost_identity_hash TEXT;
CREATE TABLE IF NOT EXISTS subscription_cost_migration_boundary (
    id SMALLINT PRIMARY KEY CHECK (id=1),
    started_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO subscription_cost_migration_boundary(id) VALUES(1) ON CONFLICT DO NOTHING;

-- Register completed upstream work before it enters deferred accounting. A row
-- is settled only by a matching durable usage log; no timeout clears uncertainty.
CREATE TABLE IF NOT EXISTS subscription_cost_pending_usage (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL,
    identity_hash TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    utilization DOUBLE PRECISION,
    reset_at TIMESTAMPTZ,
    usage_log_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS idx_subscription_cost_pending_account
    ON subscription_cost_pending_usage(account_id,identity_hash,started_at,observed_at);
ALTER TABLE IF EXISTS usage_logs ADD COLUMN IF NOT EXISTS account_cost_pending_id BIGINT;
DO $$ BEGIN IF to_regclass('usage_logs') IS NOT NULL THEN
 CREATE INDEX IF NOT EXISTS idx_usage_logs_account_cost_pending ON usage_logs(account_cost_pending_id) WHERE account_cost_pending_id IS NOT NULL;
 END IF; END $$;

CREATE INDEX IF NOT EXISTS idx_subscription_cost_pending_retention ON subscription_cost_pending_usage(observed_at,id);
