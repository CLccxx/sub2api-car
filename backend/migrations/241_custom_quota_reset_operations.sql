CREATE TABLE IF NOT EXISTS quota_reset_operations (
    account_id BIGINT NOT NULL,
    request_id TEXT NOT NULL CHECK (btrim(request_id) <> ''),
    credit_id TEXT NOT NULL DEFAULT '',
    identity_hash TEXT NOT NULL DEFAULT '',
    dispatch_count BIGINT NOT NULL DEFAULT 1 CHECK (dispatch_count > 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (account_id, request_id),
    CHECK (
        (status = 'pending' AND result IS NULL AND completed_at IS NULL)
        OR (status IN ('succeeded', 'failed') AND result IS NOT NULL AND completed_at IS NOT NULL)
    )
);

-- Older unresolved intents without an identity stay blocked rather than guessing.
ALTER TABLE quota_reset_operations ADD COLUMN IF NOT EXISTS identity_hash TEXT NOT NULL DEFAULT '';
-- Existing intents may already have reached upstream, so never backfill zero.
ALTER TABLE quota_reset_operations ADD COLUMN IF NOT EXISTS dispatch_count BIGINT NOT NULL DEFAULT 1 CHECK (dispatch_count > 0);

-- Unknown upstream outcomes retain their pending slot, including after restart.
CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_reset_operations_pending_account
    ON quota_reset_operations (account_id) WHERE status = 'pending';
