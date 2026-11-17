-- Immutable orders are the quota ledger; no mutable balance or Ent columns.
CREATE TABLE IF NOT EXISTS carpool_quota_trade_subscription_generations (
    subscription_id BIGINT PRIMARY KEY,
    generation BIGINT NOT NULL DEFAULT 0
);
CREATE OR REPLACE FUNCTION carpool_quota_trade_subscription_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.user_id,OLD.group_id,OLD.starts_at,OLD.status,OLD.deleted_at)
        IS NOT DISTINCT FROM ROW(NEW.user_id,NEW.group_id,NEW.starts_at,NEW.status,NEW.deleted_at)
        AND (OLD.expires_at=NEW.expires_at OR (OLD.status='active' AND OLD.deleted_at IS NULL
             AND OLD.expires_at>clock_timestamp() AND NEW.expires_at>=OLD.expires_at)) THEN RETURN NULL; END IF;
    INSERT INTO carpool_quota_trade_subscription_generations(subscription_id,generation)
        VALUES(COALESCE(NEW.id,OLD.id),1) ON CONFLICT(subscription_id) DO UPDATE SET generation=carpool_quota_trade_subscription_generations.generation+1;
    RETURN NULL;
END; $$;
DROP TRIGGER IF EXISTS carpool_quota_trade_subscription_changed ON user_subscriptions;
CREATE TRIGGER carpool_quota_trade_subscription_changed AFTER UPDATE OF user_id,group_id,starts_at,status,deleted_at,expires_at OR DELETE
    ON user_subscriptions FOR EACH ROW EXECUTE FUNCTION carpool_quota_trade_subscription_changed();
CREATE OR REPLACE FUNCTION carpool_quota_trade_user_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.status,OLD.deleted_at) IS NOT DISTINCT FROM ROW(NEW.status,NEW.deleted_at) THEN RETURN NULL; END IF;
    INSERT INTO carpool_quota_trade_subscription_generations(subscription_id,generation)
        SELECT id,1 FROM user_subscriptions WHERE user_id=OLD.id
        ON CONFLICT(subscription_id) DO UPDATE SET generation=carpool_quota_trade_subscription_generations.generation+1;
    RETURN NULL;
END; $$;
DROP TRIGGER IF EXISTS carpool_quota_trade_user_changed ON users;
CREATE TRIGGER carpool_quota_trade_user_changed AFTER UPDATE OF status,deleted_at ON users
    FOR EACH ROW EXECUTE FUNCTION carpool_quota_trade_user_changed();

CREATE TABLE IF NOT EXISTS carpool_quota_trades (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL,
    group_name TEXT NOT NULL,
    account_id BIGINT NOT NULL,
    identity_hash TEXT NOT NULL,
    source_revision BIGINT NOT NULL,
    binding_revision BIGINT NOT NULL,
    cycle_id TEXT NOT NULL,
    seller_user_id BIGINT NOT NULL,
    seller_name TEXT NOT NULL,
    seller_subscription_id BIGINT NOT NULL,
    seller_generation BIGINT NOT NULL,
    buyer_user_id BIGINT,
    buyer_name TEXT,
    buyer_subscription_id BIGINT,
    buyer_generation BIGINT,
    amount_cents BIGINT NOT NULL CHECK(amount_cents>0 AND amount_cents<=1000000000000),
    request_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'open' CHECK(status IN('open','reserved','completed','withdrawn','expired')),
    terms_version TEXT NOT NULL,
    terms_accepted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    reserved_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(seller_user_id,request_id),
    CHECK((status IN('reserved','completed') AND buyer_user_id IS NOT NULL AND buyer_subscription_id IS NOT NULL AND buyer_generation IS NOT NULL)
       OR status IN('open','withdrawn','expired')),
    CHECK(buyer_user_id IS NULL OR buyer_user_id<>seller_user_id)
);
CREATE INDEX IF NOT EXISTS idx_carpool_quota_trade_market ON carpool_quota_trades(group_id,cycle_id,status,id DESC);
CREATE INDEX IF NOT EXISTS idx_carpool_quota_trade_seller ON carpool_quota_trades(seller_subscription_id,id DESC);
CREATE INDEX IF NOT EXISTS idx_carpool_quota_trade_buyer ON carpool_quota_trades(buyer_subscription_id,id DESC);
CREATE TABLE IF NOT EXISTS carpool_quota_trade_events (
    id BIGSERIAL PRIMARY KEY,
    trade_id BIGINT NOT NULL REFERENCES carpool_quota_trades(id),
    action TEXT NOT NULL CHECK(action IN('create','reserve','confirm','cancel-reservation','withdraw','expire')),
    actor_user_id BIGINT,
    actor_name TEXT NOT NULL,
    is_admin BOOLEAN NOT NULL DEFAULT FALSE,
    buyer_user_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS idx_carpool_quota_trade_events_order ON carpool_quota_trade_events(trade_id,id);

CREATE INDEX IF NOT EXISTS idx_carpool_quota_trade_completed_cycle
    ON carpool_subscription_reset_events(account_id,identity_hash,id DESC) WHERE status='completed';

-- Identity is the same trimmed SHA256 used by CarpoolResetIdentityHash.
-- Cycle changes ONLY on confirmed account resets, never on personal windows.
CREATE OR REPLACE VIEW carpool_quota_trade_subscription_state AS
SELECT s.id AS subscription_id,s.user_id,s.group_id,g.name AS group_name,u.username AS user_name,
       a.id AS account_id,encode(sha256(convert_to(btrim(a.credentials->>'chatgpt_account_id'),'UTF8')),'hex') AS identity_hash,
       a.id::text||':'||COALESCE(e.id,0)::text AS cycle_id,
       COALESCE(e.observed_at,'-infinity'::timestamptz) AS cycle_observed_at,
       COALESCE(gen.generation,0) AS generation,
       (SELECT count(*) FROM carpool_reset_account_changes c WHERE c.account_id=a.id) AS source_revision,
       (SELECT count(*) FROM carpool_reset_group_changes c WHERE c.group_id=g.id) AS binding_revision,
       g.weekly_limit_usd AS base_limit_usd,s.weekly_usage_usd AS used_usd,
       (s.deleted_at IS NULL AND s.status='active' AND s.starts_at<=clock_timestamp() AND s.expires_at>clock_timestamp()
        AND u.deleted_at IS NULL AND u.status='active' AND g.deleted_at IS NULL AND g.status='active'
        AND g.platform='openai' AND g.subscription_type='subscription' AND g.carpool_members BETWEEN 2 AND 5
        AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth' AND a.status='active' AND a.parent_account_id IS NULL
        AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
        AND btrim(COALESCE(a.credentials->>'chatgpt_account_id',''))<>''
        AND NOT EXISTS(SELECT 1 FROM account_groups other JOIN accounts b ON b.id=other.account_id
                       WHERE other.group_id=g.id AND b.deleted_at IS NULL AND b.id<>a.id)) AS eligible
FROM user_subscriptions s JOIN users u ON u.id=s.user_id JOIN groups g ON g.id=s.group_id
JOIN account_groups ag ON ag.group_id=g.id JOIN accounts a ON a.id=ag.account_id
LEFT JOIN carpool_quota_trade_subscription_generations gen ON gen.subscription_id=s.id
LEFT JOIN LATERAL (SELECT ev.id,ev.observed_at FROM carpool_subscription_reset_events ev
    WHERE ev.account_id=a.id AND ev.identity_hash=encode(sha256(convert_to(btrim(a.credentials->>'chatgpt_account_id'),'UTF8')),'hex')
      AND ev.status='completed' ORDER BY ev.id DESC LIMIT 1) e ON TRUE;

CREATE OR REPLACE VIEW carpool_quota_trade_entitlements AS
SELECT st.subscription_id,st.group_id,st.account_id,st.cycle_id,st.base_limit_usd,st.used_usd,
       COALESCE(sum(CASE WHEN t.status='completed' THEN CASE WHEN t.buyer_subscription_id=st.subscription_id THEN t.amount_cents ELSE -t.amount_cents END ELSE 0 END),0) AS adjustment_cents,
       COALESCE(sum(CASE WHEN t.status IN('open','reserved') AND t.seller_subscription_id=st.subscription_id THEN t.amount_cents ELSE 0 END),0) AS frozen_cents
FROM carpool_quota_trade_subscription_state st LEFT JOIN carpool_quota_trades t
 ON t.group_id=st.group_id AND t.account_id=st.account_id AND t.identity_hash=st.identity_hash AND t.cycle_id=st.cycle_id
 AND (t.status='completed' OR (t.source_revision=st.source_revision AND t.binding_revision=st.binding_revision))
 -- Pending intents require the exact term; settled quota lasts the account cycle.
 AND ((t.seller_subscription_id=st.subscription_id AND t.seller_user_id=st.user_id AND (t.status='completed' OR t.seller_generation=st.generation))
   OR (t.buyer_subscription_id=st.subscription_id AND t.buyer_user_id=st.user_id AND (t.status='completed' OR t.buyer_generation=st.generation)))
WHERE st.eligible
GROUP BY st.subscription_id,st.group_id,st.account_id,st.cycle_id,st.base_limit_usd,st.used_usd;

-- Durable invalidation even if the process dies before a cache call.
CREATE OR REPLACE FUNCTION carpool_quota_trade_invalidate_auth() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO auth_cache_invalidation_outbox(cache_key)
      SELECT encode(sha256(convert_to(k.key,'UTF8')),'hex') FROM api_keys k
      WHERE k.group_id=NEW.group_id AND k.deleted_at IS NULL ON CONFLICT DO NOTHING;
    RETURN NULL;
END; $$;
DROP TRIGGER IF EXISTS carpool_quota_trade_invalidate_auth ON carpool_quota_trades;
CREATE TRIGGER carpool_quota_trade_invalidate_auth AFTER INSERT OR UPDATE OF status ON carpool_quota_trades
    FOR EACH ROW EXECUTE FUNCTION carpool_quota_trade_invalidate_auth();

-- No timer expiry. Only completion of an actual account reset expires open work.
CREATE OR REPLACE FUNCTION carpool_quota_trade_cycle_completed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status<>'completed' OR (TG_OP='UPDATE' AND OLD.status='completed') THEN RETURN NULL; END IF;
    IF EXISTS(SELECT 1 FROM carpool_subscription_reset_events WHERE account_id=NEW.account_id AND identity_hash=NEW.identity_hash
              AND status='completed' AND id>NEW.id) THEN RETURN NULL; END IF;
    WITH changed AS (UPDATE carpool_quota_trades SET status='expired',updated_at=clock_timestamp()
      WHERE account_id=NEW.account_id AND identity_hash=NEW.identity_hash AND cycle_id<>NEW.account_id::text||':'||NEW.id::text
        AND status IN('open','reserved') RETURNING id,buyer_user_id)
    INSERT INTO carpool_quota_trade_events(trade_id,action,actor_name,buyer_user_id)
      SELECT id,'expire','system',buyer_user_id FROM changed;
    RETURN NULL;
END; $$;
DROP TRIGGER IF EXISTS carpool_quota_trade_cycle_completed ON carpool_subscription_reset_events;
CREATE TRIGGER carpool_quota_trade_cycle_completed AFTER INSERT OR UPDATE OF status ON carpool_subscription_reset_events
    FOR EACH ROW EXECUTE FUNCTION carpool_quota_trade_cycle_completed();
