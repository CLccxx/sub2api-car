-- Store the configurable carpool bonus without changing any historical quota cap.
-- Keep eight decimal places so the CHECK rejects sub-cent amounts instead of
-- silently rounding them to cents as numeric(20,2) would.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS carpool_bonus_usd DECIMAL(20,8) NOT NULL DEFAULT 100
        CHECK (carpool_bonus_usd BETWEEN 0 AND 10000000000
            AND carpool_bonus_usd = round(carpool_bonus_usd, 2));
