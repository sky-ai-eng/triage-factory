-- +goose Up
-- runs.cred_pubkey: the per-run credential sidecar's X25519 public key
-- (base64), written by the executor when it parks the run in
-- status='awaiting_credentials' and read by the brain at seal time — the
-- bundle must be sealed to the sidecar that will actually open it, not to
-- the claiming instance's per-boot key. Claim-scoped ownership metadata:
-- it rides with executor_id/boot_epoch and is cleared wherever they are
-- (a queued row has no owner and no key), so a requeued/reset run never
-- carries a stale key the brain could mistakenly seal to. NULL in local
-- mode (always role=all, which never parks a run in this status); the
-- column exists here for store-interface + conformance-test symmetry with
-- Postgres.
ALTER TABLE runs ADD COLUMN cred_pubkey TEXT;

-- +goose Down
SELECT 'down not supported';
