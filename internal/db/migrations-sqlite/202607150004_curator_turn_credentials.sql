-- +goose Up
-- curator_turn_credentials: the curator-turn analog of
-- run_credentials — the sealed per-turn credential bundle channel. A homed
-- curator turn is a curator_requests row (not a runs row), so the run-keyed
-- handshake can't carry it; this is the column-for-column mirror keyed on the
-- request id instead. See the Postgres baseline's curator_turn_credentials
-- comment for the full design. Never populated in local mode (forced role=all,
-- which resolves credentials directly against the keychain; the bundle path is
-- executor-role-only) but lands here anyway for store-interface + conformance-
-- test symmetry across dialects, the same rule run_credentials followed.

-- curator_requests.cred_pubkey: the per-turn sidecar's X25519 public key
-- (base64), the curator-turn analog of runs.cred_pubkey. Written by the home
-- executor when it stands the turn's credential sidecar up and read by the
-- brain at seal time. NOT NULL DEFAULT '' — empty is "no sidecar published
-- yet" (the untouched local / role=all path). Recorded only while the turn is
-- non-terminal so a finished turn never carries a stale key.
ALTER TABLE curator_requests ADD COLUMN cred_pubkey TEXT NOT NULL DEFAULT '';

CREATE TABLE curator_turn_credentials (
    request_id  TEXT PRIMARY KEY REFERENCES curator_requests(id) ON DELETE CASCADE,
    org_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    executor_id TEXT NOT NULL,
    boot_epoch  INTEGER NOT NULL,
    sealed      BLOB NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
SELECT 'down not supported';
