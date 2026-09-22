-- +goose Up
ALTER TABLE claims ADD COLUMN lease_expires_at DATETIME;
-- A claim live across the upgrade belongs to a process that is gone (the
-- identity file lock proves it). It gets an already-expired lease so the
-- live-claim invariant holds and ordinary expiry handling recovers it.
UPDATE claims SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 seconds') WHERE released_at IS NULL;
CREATE INDEX idx_claims_live_expiry ON claims (lease_expires_at) WHERE released_at IS NULL;
-- +goose Down
SELECT 'down not supported';
