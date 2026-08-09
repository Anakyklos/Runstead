-- 0010_governor_manual_reserve.sql
-- The manual reserve is part of the allowance policy projection (#58): it
-- only applies to published-quota allowances and is zero everywhere else.
-- Existing rows keep reserve 0 (legacy projections predating this column),
-- which is the correct default for any non-numeric allowance and was never
-- used by the CLI's published-quota resume path (the reserve is derived from
-- the reconstructed profile there and checked for consistency).
ALTER TABLE governor_state ADD COLUMN manual_reserve_ceiling INTEGER NOT NULL DEFAULT 0;
