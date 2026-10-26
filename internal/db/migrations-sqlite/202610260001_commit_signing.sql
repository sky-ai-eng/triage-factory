-- +goose Up
-- org_commit_signers: the GitHub user account an org's agent commits are
-- authored, committed and signed as, one row per org. The key pair is TF's:
-- public_key is the half the admin adds to the account as a signing key, and
-- the private half is the keychain secret github_commit_signing_key, never a
-- column. pending_public_key is a rotation's new key, NULL outside one.
--
-- The account is identified by github_user_id; login and name are display
-- values nothing keys on. host is the GitHub the account is on, the org's host
-- at registration. email_override NULL means commits use the derived noreply
-- address, which exists only on github.com, so a signer on any other host
-- needs one. status ('pending' | 'active' | 'broken') and status_reason are
-- app-validated, as in Postgres.
CREATE TABLE org_commit_signers (
    org_id             TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    host               TEXT NOT NULL CHECK (host <> ''),
    github_user_id     INTEGER NOT NULL,
    login              TEXT NOT NULL,
    name               TEXT,
    email_override     TEXT,
    public_key         TEXT NOT NULL,
    pending_public_key TEXT,
    status             TEXT NOT NULL DEFAULT 'pending',
    status_reason      TEXT,
    status_since       DATETIME NOT NULL,
    checked_at         DATETIME,
    created_at         DATETIME NOT NULL,
    updated_at         DATETIME NOT NULL,
    created_by         TEXT REFERENCES users(id) ON DELETE SET NULL
);

-- commit_signatures: one row per commit signature an agent's git asked for —
-- the signing log, and what the signing budgets count, so no counter is
-- stored. A local daemon records a signature already made, so its rows start
-- 'signed'; the 'requested' and 'signing' states and the claimed_by and
-- retry_after_seconds columns serve multi mode's control-plane queue and stay
-- unused here. conversation_id and claim_id null on the conversation's purge,
-- so the log outlives it. The columns are described on the Postgres table.
CREATE TABLE commit_signatures (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    conversation_id     TEXT REFERENCES conversations(id) ON DELETE SET NULL,
    claim_id            TEXT REFERENCES claims(id) ON DELETE SET NULL,
    boot_epoch          INTEGER NOT NULL,
    signer              TEXT NOT NULL,
    signer_host         TEXT NOT NULL CHECK (signer_host <> ''),
    signer_user_id      INTEGER NOT NULL,
    public_key          TEXT,
    digest              TEXT NOT NULL,
    state               TEXT NOT NULL,
    refusal_reason      TEXT,
    retry_after_seconds INTEGER,
    signature           TEXT,
    claimed_by          TEXT,
    signed_at           DATETIME,
    commit_sha          TEXT,
    repository_id       TEXT REFERENCES repositories(id) ON DELETE SET NULL,
    pushed_at           DATETIME,
    verification_reason TEXT,
    checked_at          DATETIME,
    created_at          DATETIME NOT NULL
);

-- The two signing budgets: an org's signatures in the last minute, and one
-- claim's in total.
CREATE INDEX commit_signatures_org_signed_idx ON commit_signatures (org_id, signed_at) WHERE state = 'signed';
CREATE INDEX commit_signatures_claim_signed_idx ON commit_signatures (claim_id) WHERE state = 'signed';

-- +goose Down
SELECT 'down not supported';
