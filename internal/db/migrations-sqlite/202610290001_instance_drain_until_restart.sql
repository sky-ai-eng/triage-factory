-- +goose Up
-- instances.drain_until_epoch: a drain that ends at the instance's next
-- restart. It holds the boot_epoch that was current when the drain was set,
-- and the instance is draining while it equals boot_epoch. Register bumps
-- boot_epoch on every boot and leaves this column alone, so the drain lapses
-- by itself, and a value left behind never matches again because epochs only
-- increase. `draining` stays the operator's drain, which survives restarts;
-- undraining clears both. NULL when no such drain was ever set.
ALTER TABLE instances ADD COLUMN drain_until_epoch INTEGER;

-- +goose Down
SELECT 'down not supported';
