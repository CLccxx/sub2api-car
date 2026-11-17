-- Record the user-entered TOTAL sale price in CNY integer cents, not a quota
-- unit price. Keep pre-existing orders NULL; never infer a historical price.
ALTER TABLE carpool_quota_trades
    ADD COLUMN IF NOT EXISTS sale_price_cents BIGINT NULL
    CHECK (sale_price_cents > 0 AND sale_price_cents <= 1000000000000);
