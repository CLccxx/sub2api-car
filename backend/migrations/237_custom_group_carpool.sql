ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS carpool_members INTEGER NOT NULL DEFAULT 0
        CHECK (carpool_members = 0 OR carpool_members BETWEEN 2 AND 5),
    ADD COLUMN IF NOT EXISTS carpool_quota_synced_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_groups_carpool_active
    ON groups (id) WHERE carpool_members > 0 AND deleted_at IS NULL;
