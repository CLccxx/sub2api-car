CREATE TABLE IF NOT EXISTS carpool_reset_votes (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    created_by BIGINT NOT NULL,
    creation_key UUID NOT NULL,
    eligibility JSONB NOT NULL,
    observation JSONB NOT NULL,
    required_count INTEGER NOT NULL CHECK (required_count BETWEEN 2 AND 5),
    status TEXT NOT NULL DEFAULT 'voting' CHECK (status IN ('voting','executing','awaiting_confirmation','applying','completed','expired','invalidated','failed')),
    reason TEXT,
    expires_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()+interval '24 hours',
    credit_id TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL DEFAULT '',
    dispatched_at TIMESTAMPTZ,
    succeeded_at TIMESTAMPTZ,
    event_id BIGINT REFERENCES carpool_subscription_reset_events(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (created_by,creation_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_carpool_vote_active_account ON carpool_reset_votes(account_id)
    WHERE status IN ('voting','executing','awaiting_confirmation','applying');
CREATE TABLE IF NOT EXISTS carpool_reset_vote_approvals (
    proposal_id BIGINT NOT NULL REFERENCES carpool_reset_votes(id),
    user_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(proposal_id,user_id)
);
CREATE TABLE IF NOT EXISTS carpool_reset_vote_requests (
    created_by BIGINT NOT NULL,
    creation_key UUID NOT NULL,
    proposal_id BIGINT NOT NULL REFERENCES carpool_reset_votes(id),
    PRIMARY KEY(created_by,creation_key)
);
CREATE TABLE IF NOT EXISTS carpool_reset_vote_targets (
    proposal_id BIGINT NOT NULL REFERENCES carpool_reset_votes(id),
    subscription_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(proposal_id,subscription_id)
);
ALTER TABLE carpool_subscription_reset_events DROP CONSTRAINT IF EXISTS carpool_subscription_reset_events_reason_check;
ALTER TABLE carpool_subscription_reset_events ADD CONSTRAINT carpool_subscription_reset_events_reason_check CHECK (reason IN ('window','receipt','vote'));
CREATE TABLE IF NOT EXISTS carpool_vote_membership_changes (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_carpool_vote_membership_group ON carpool_vote_membership_changes(group_id);
CREATE OR REPLACE FUNCTION carpool_vote_subscription_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.user_id,OLD.group_id,OLD.status,OLD.deleted_at,OLD.starts_at)
        IS NOT DISTINCT FROM ROW(NEW.user_id,NEW.group_id,NEW.status,NEW.deleted_at,NEW.starts_at)
        AND OLD.status='active' AND OLD.deleted_at IS NULL AND OLD.expires_at>clock_timestamp()
        AND NEW.expires_at>=OLD.expires_at THEN
        RETURN NULL;
    END IF;
    IF TG_OP='UPDATE' AND ROW(OLD.user_id,OLD.group_id,OLD.status,OLD.deleted_at,OLD.starts_at,OLD.expires_at)
        IS NOT DISTINCT FROM ROW(NEW.user_id,NEW.group_id,NEW.status,NEW.deleted_at,NEW.starts_at,NEW.expires_at) THEN
        RETURN NULL;
    END IF;
    INSERT INTO carpool_vote_membership_changes(group_id)
        SELECT DISTINCT g.id FROM groups g WHERE g.id IN (OLD.group_id,NEW.group_id) AND g.carpool_members BETWEEN 2 AND 5;
    RETURN NULL;
END;
$$;
CREATE OR REPLACE FUNCTION carpool_vote_user_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='UPDATE' AND ROW(OLD.status,OLD.deleted_at) IS NOT DISTINCT FROM ROW(NEW.status,NEW.deleted_at) THEN
        RETURN NULL;
    END IF;
    INSERT INTO carpool_vote_membership_changes(group_id)
        SELECT DISTINCT g.id FROM groups g JOIN user_subscriptions s ON s.group_id=g.id
        WHERE s.user_id IN (OLD.id,NEW.id) AND g.carpool_members BETWEEN 2 AND 5;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS carpool_vote_subscription_changed ON user_subscriptions;
CREATE TRIGGER carpool_vote_subscription_changed AFTER INSERT OR UPDATE OF user_id,group_id,status,deleted_at,starts_at,expires_at OR DELETE
    ON user_subscriptions FOR EACH ROW EXECUTE FUNCTION carpool_vote_subscription_changed();
DROP TRIGGER IF EXISTS carpool_vote_user_changed ON users;
CREATE TRIGGER carpool_vote_user_changed AFTER UPDATE OF status,deleted_at OR DELETE
    ON users FOR EACH ROW EXECUTE FUNCTION carpool_vote_user_changed();
