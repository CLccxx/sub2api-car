-- Keep the singleton as the existing serialization lock. Observations belong to
-- accounts; append-only global revisions retain the meaning of old snapshots.
CREATE TABLE IF NOT EXISTS carpool_reset_account_monitors (
    account_id BIGINT PRIMARY KEY,
    group_id BIGINT NOT NULL,
    identity_hash TEXT NOT NULL,
    source_revision BIGINT NOT NULL,
    scope_version INTEGER NOT NULL DEFAULT 0,
    baseline JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO carpool_reset_account_monitors(account_id,group_id,identity_hash,source_revision,baseline,updated_at)
    SELECT account_id,group_id,identity_hash,source_revision,baseline,updated_at
    FROM carpool_reset_monitor WHERE account_id IS NOT NULL AND group_id IS NOT NULL AND identity_hash IS NOT NULL
    ON CONFLICT(account_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS carpool_reset_account_changes (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_carpool_reset_account_changes_account ON carpool_reset_account_changes(account_id);
ALTER TABLE carpool_subscription_reset_events ADD COLUMN IF NOT EXISTS scope_version INTEGER NOT NULL DEFAULT 0;
-- Promote only proven-current legacy baselines at the migration boundary. This
-- preserves detection even if an unrelated account changes before the first tick.
UPDATE carpool_reset_account_monitors m SET scope_version=1,source_revision=0
    WHERE m.scope_version=0 AND m.baseline IS NOT NULL
    AND m.source_revision=(SELECT count(*) FROM carpool_reset_source_changes)
    AND NOT EXISTS(SELECT 1 FROM carpool_reset_account_changes c WHERE c.account_id=m.account_id);

CREATE TABLE IF NOT EXISTS carpool_reset_group_changes (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_carpool_reset_group_changes_group ON carpool_reset_group_changes(group_id);
-- NULL marks historical global snapshots. Never bless them with today's binding:
-- an ordinary group may have moved accounts before this migration.
ALTER TABLE carpool_subscription_reset_targets ADD COLUMN IF NOT EXISTS binding_revision BIGINT;
ALTER TABLE carpool_reset_vote_targets ADD COLUMN IF NOT EXISTS binding_revision BIGINT;
ALTER TABLE carpool_reset_votes DROP CONSTRAINT IF EXISTS carpool_reset_votes_required_count_check;
ALTER TABLE carpool_reset_votes ADD CONSTRAINT carpool_reset_votes_required_count_check CHECK(required_count >= 1);

-- Undispatched proposals used a different electorate. They cannot authorize a
-- spend under the new rules. Preserve approvals, request keys and outcomes.
UPDATE carpool_reset_votes SET status='invalidated',reason='source_changed',updated_at=clock_timestamp()
    WHERE dispatched_at IS NULL AND status IN ('voting','executing')
    AND COALESCE((eligibility->>'ScopeVersion')::int,0)=0;
UPDATE quota_reset_operations o SET status='failed',result='{"code":"vote_invalidated","windows_reset":0}'::jsonb,completed_at=clock_timestamp()
    FROM carpool_reset_votes p WHERE o.account_id=p.account_id AND o.request_id=p.request_id
    AND o.status='pending' AND p.dispatched_at IS NULL AND p.status='invalidated'
    AND COALESCE((p.eligibility->>'ScopeVersion')::int,0)=0;
-- Dispatched legacy proposals remain recoverable from their durable outcome;
-- ScopeVersion=0 is deliberately retained to prohibit uncertain HTTP replays.

CREATE OR REPLACE FUNCTION carpool_reset_account_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND
       ROW(OLD.platform,OLD.type,OLD.status,OLD.parent_account_id,OLD.deleted_at,OLD.auto_pause_on_expired,OLD.expires_at,
           btrim(COALESCE(OLD.credentials->>'chatgpt_account_id',''))) IS NOT DISTINCT FROM
       ROW(NEW.platform,NEW.type,NEW.status,NEW.parent_account_id,NEW.deleted_at,NEW.auto_pause_on_expired,NEW.expires_at,
           btrim(COALESCE(NEW.credentials->>'chatgpt_account_id',''))) THEN RETURN NULL; END IF;
    PERFORM carpool_reset_record_source_change();
    INSERT INTO carpool_reset_account_changes(account_id)
        SELECT DISTINCT ag.account_id FROM account_groups ag JOIN groups g ON g.id=ag.group_id
        WHERE g.platform='openai' AND g.subscription_type='subscription'
        AND ag.group_id IN(SELECT group_id FROM account_groups WHERE account_id=OLD.id)
        UNION SELECT OLD.id;
    INSERT INTO carpool_reset_group_changes(group_id) SELECT DISTINCT group_id FROM account_groups WHERE account_id=OLD.id;
    RETURN NULL;
END; $$;
CREATE OR REPLACE FUNCTION carpool_reset_group_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.platform,OLD.subscription_type,OLD.status,OLD.carpool_members,OLD.deleted_at)
       IS NOT DISTINCT FROM ROW(NEW.platform,NEW.subscription_type,NEW.status,NEW.carpool_members,NEW.deleted_at) THEN RETURN NULL; END IF;
    PERFORM carpool_reset_record_source_change();
    IF (OLD.platform='openai' AND OLD.subscription_type='subscription') OR (NEW.platform='openai' AND NEW.subscription_type='subscription') THEN
        INSERT INTO carpool_reset_account_changes(account_id) SELECT DISTINCT account_id FROM account_groups WHERE group_id=COALESCE(NEW.id,OLD.id);
    END IF;
    INSERT INTO carpool_reset_group_changes(group_id) VALUES(COALESCE(NEW.id,OLD.id));
    RETURN NULL;
END; $$;
CREATE OR REPLACE FUNCTION carpool_reset_binding_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.account_id,OLD.group_id) IS NOT DISTINCT FROM ROW(NEW.account_id,NEW.group_id) THEN RETURN NULL; END IF;
    PERFORM carpool_reset_record_source_change();
    IF EXISTS(SELECT 1 FROM groups WHERE id IN(OLD.group_id,NEW.group_id) AND platform='openai' AND subscription_type='subscription') THEN
        INSERT INTO carpool_reset_account_changes(account_id)
            SELECT DISTINCT ag.account_id FROM account_groups ag JOIN groups g ON g.id=ag.group_id
            WHERE ag.group_id IN(OLD.group_id,NEW.group_id) AND g.platform='openai' AND g.subscription_type='subscription'
            UNION SELECT OLD.account_id WHERE OLD.account_id IS NOT NULL
            UNION SELECT NEW.account_id WHERE NEW.account_id IS NOT NULL;
    END IF;
    INSERT INTO carpool_reset_group_changes(group_id)
        SELECT DISTINCT g FROM unnest(ARRAY[OLD.group_id,NEW.group_id]) AS g WHERE g IS NOT NULL;
    RETURN NULL;
END; $$;
CREATE OR REPLACE FUNCTION carpool_vote_subscription_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.user_id,OLD.group_id,OLD.status,OLD.deleted_at,OLD.starts_at)
        IS NOT DISTINCT FROM ROW(NEW.user_id,NEW.group_id,NEW.status,NEW.deleted_at,NEW.starts_at)
        AND OLD.status='active' AND OLD.deleted_at IS NULL AND OLD.expires_at>clock_timestamp()
        AND NEW.expires_at>=OLD.expires_at THEN RETURN NULL; END IF;
    IF TG_OP='UPDATE' AND ROW(OLD.user_id,OLD.group_id,OLD.status,OLD.deleted_at,OLD.starts_at,OLD.expires_at)
        IS NOT DISTINCT FROM ROW(NEW.user_id,NEW.group_id,NEW.status,NEW.deleted_at,NEW.starts_at,NEW.expires_at) THEN RETURN NULL; END IF;
    INSERT INTO carpool_vote_membership_changes(group_id)
        SELECT DISTINCT g.id FROM groups g WHERE g.id IN(OLD.group_id,NEW.group_id) AND g.subscription_type='subscription';
    RETURN NULL;
END; $$;
CREATE OR REPLACE FUNCTION carpool_vote_user_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.status,OLD.deleted_at) IS NOT DISTINCT FROM ROW(NEW.status,NEW.deleted_at) THEN RETURN NULL; END IF;
    INSERT INTO carpool_vote_membership_changes(group_id)
        SELECT DISTINCT g.id FROM groups g JOIN user_subscriptions s ON s.group_id=g.id
        WHERE s.user_id IN(OLD.id,NEW.id) AND g.subscription_type='subscription';
    RETURN NULL;
END; $$;
