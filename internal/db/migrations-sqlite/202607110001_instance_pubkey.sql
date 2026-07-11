-- +goose Up
-- instances.pubkey (TFAC-614): this boot's ephemeral X25519 public key
-- (base64), minted in-memory at process start and never persisted — a
-- restart mints a fresh one. Written ONLY by Register's initial INSERT and
-- its ON CONFLICT re-stamp (every restart gets a new key alongside the new
-- boot_epoch); the heartbeat never touches it, same non-write rule as
-- draining/labels_json, but for the opposite reason — those survive a
-- restart on purpose, this one must NOT (an old-epoch key sealing a bundle
-- nobody can decrypt anymore is exactly the crash window TFAC-614's epoch
-- check exists to catch). NULL for a control/all row that never provisions
-- or claims runs — which in local mode (always role=all) is every row; the
-- column exists here only for store-interface + conformance-test symmetry
-- with Postgres, not because local mode ever seals a bundle.
ALTER TABLE instances ADD COLUMN pubkey TEXT;

-- +goose Down
SELECT 'down not supported';
