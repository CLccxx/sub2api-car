CREATE TABLE IF NOT EXISTS carpool_reset_monitor (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    group_id BIGINT,
    account_id BIGINT,
    identity_hash TEXT,
    source_revision BIGINT NOT NULL DEFAULT 0,
    baseline JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS carpool_account_reset_receipts (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL,
    identity_hash TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    windows_reset INTEGER NOT NULL CHECK (windows_reset > 0),
    UNIQUE (account_id, identity_hash, request_hash)
);
CREATE INDEX IF NOT EXISTS idx_carpool_reset_receipt_lookup
    ON carpool_account_reset_receipts (account_id, identity_hash, id DESC);

CREATE TABLE IF NOT EXISTS carpool_subscription_reset_events (
    id BIGSERIAL PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE,
    group_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    identity_hash TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('window', 'receipt')),
    source_revision BIGINT NOT NULL DEFAULT 0,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'completed', 'cancelled')),
    reset_count INTEGER NOT NULL DEFAULT 0,
    skipped_count INTEGER NOT NULL DEFAULT 0,
    cancel_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_carpool_reset_pending
    ON carpool_subscription_reset_events (id) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS carpool_subscription_reset_targets (
    event_id BIGINT NOT NULL REFERENCES carpool_subscription_reset_events(id) ON DELETE CASCADE,
    subscription_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    reset_applied BOOLEAN NOT NULL DEFAULT FALSE,
    cache_invalidated_at TIMESTAMPTZ,
    PRIMARY KEY (event_id, subscription_id)
);
CREATE INDEX IF NOT EXISTS idx_carpool_reset_cache_pending
    ON carpool_subscription_reset_targets (event_id, subscription_id)
    WHERE reset_applied AND cache_invalidated_at IS NULL;

-- Append-only generations catch A -> B -> A transitions and in-flight reads.
-- Triggers do not lock the singleton or events inside business transactions.
CREATE TABLE IF NOT EXISTS carpool_reset_source_changes (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE OR REPLACE FUNCTION carpool_reset_record_source_change() RETURNS VOID
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO carpool_reset_source_changes DEFAULT VALUES;
END;
$$;

CREATE OR REPLACE FUNCTION carpool_reset_account_changed() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND
       ROW(OLD.platform,OLD.type,OLD.status,OLD.parent_account_id,OLD.deleted_at,OLD.auto_pause_on_expired,OLD.expires_at,
           btrim(COALESCE(OLD.credentials->>'chatgpt_account_id',''))) IS NOT DISTINCT FROM
       ROW(NEW.platform,NEW.type,NEW.status,NEW.parent_account_id,NEW.deleted_at,NEW.auto_pause_on_expired,NEW.expires_at,
           btrim(COALESCE(NEW.credentials->>'chatgpt_account_id',''))) THEN
        RETURN NULL;
    END IF;
    IF EXISTS (
        SELECT 1 FROM carpool_reset_monitor m WHERE m.id=1 AND
        (m.account_id=OLD.id OR EXISTS (
            SELECT 1 FROM account_groups ag WHERE ag.group_id=m.group_id AND ag.account_id=OLD.id
        ))
    ) OR EXISTS (
        SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
        WHERE ag.account_id=OLD.id AND g.carpool_members BETWEEN 2 AND 5
        AND g.platform='openai' AND g.subscription_type='subscription'
    ) THEN
        PERFORM carpool_reset_record_source_change();
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION carpool_reset_group_changed() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND
       ROW(OLD.platform,OLD.subscription_type,OLD.status,OLD.carpool_members,OLD.deleted_at) IS NOT DISTINCT FROM
       ROW(NEW.platform,NEW.subscription_type,NEW.status,NEW.carpool_members,NEW.deleted_at) THEN
        RETURN NULL;
    END IF;
    IF (
        EXISTS (SELECT 1 FROM carpool_reset_monitor WHERE id=1 AND group_id IN (OLD.id,NEW.id)) OR
        (OLD.deleted_at IS NULL AND OLD.status='active' AND OLD.platform='openai' AND OLD.subscription_type='subscription' AND OLD.carpool_members BETWEEN 2 AND 5) OR
        (NEW.deleted_at IS NULL AND NEW.status='active' AND NEW.platform='openai' AND NEW.subscription_type='subscription' AND NEW.carpool_members BETWEEN 2 AND 5)
    ) THEN
        PERFORM carpool_reset_record_source_change();
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION carpool_reset_binding_changed() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.account_id,OLD.group_id) IS NOT DISTINCT FROM ROW(NEW.account_id,NEW.group_id) THEN
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM carpool_reset_monitor WHERE id=1 AND group_id IN (OLD.group_id,NEW.group_id)) OR
       EXISTS (SELECT 1 FROM groups WHERE id IN (OLD.group_id,NEW.group_id) AND carpool_members BETWEEN 2 AND 5
               AND platform='openai' AND subscription_type='subscription') THEN
        PERFORM carpool_reset_record_source_change();
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS carpool_reset_account_changed ON accounts;
CREATE TRIGGER carpool_reset_account_changed
    AFTER UPDATE OF platform,type,status,parent_account_id,credentials,deleted_at,auto_pause_on_expired,expires_at OR DELETE
    ON accounts FOR EACH ROW EXECUTE FUNCTION carpool_reset_account_changed();

DROP TRIGGER IF EXISTS carpool_reset_group_changed ON groups;
CREATE TRIGGER carpool_reset_group_changed
    AFTER INSERT OR UPDATE OF platform,subscription_type,status,carpool_members,deleted_at OR DELETE
    ON groups FOR EACH ROW EXECUTE FUNCTION carpool_reset_group_changed();

DROP TRIGGER IF EXISTS carpool_reset_binding_changed ON account_groups;
CREATE TRIGGER carpool_reset_binding_changed
    AFTER INSERT OR UPDATE OR DELETE
    ON account_groups FOR EACH ROW EXECUTE FUNCTION carpool_reset_binding_changed();
