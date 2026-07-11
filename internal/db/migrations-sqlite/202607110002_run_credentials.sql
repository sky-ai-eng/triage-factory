-- +goose Up
-- run_credentials (TFAC-614): the sealed per-run credential bundle channel.
-- See the Postgres baseline's run_credentials comment for the full design —
-- this table is never populated in local mode (forced role=all, which
-- resolves credentials directly against the keychain exactly as before;
-- the bundle path is executor-role-only) but lands here anyway for store-
-- interface + conformance-test symmetry across dialects.
--
-- One row per run (PK run_id), replaced wholesale on every write. boot_epoch
-- is the claiming executor's boot epoch at seal time, carried in cleartext
-- so the executor can compare it against its own current epoch before
-- attempting to unseal — never Open a bundle sealed for an earlier boot.
CREATE TABLE run_credentials (
    run_id      TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    org_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    executor_id TEXT NOT NULL,
    boot_epoch  INTEGER NOT NULL,
    sealed      BLOB NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
SELECT 'down not supported';
