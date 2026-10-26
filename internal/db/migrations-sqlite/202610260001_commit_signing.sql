-- +goose Up
-- Commit signing: an org registers a GitHub user account as its signer, and
-- every commit its agents make is authored, committed and signed as that
-- account. org_commit_signers holds the signer, and commit_signatures the log
-- of every signature a run asks for. The columns are described on the Postgres
-- tables; what differs in local mode is noted here.

-- org_commit_signers: one row per org. The key pair is TF's: public_key is the
-- half the admin adds to the account as a signing key, and the private half is
-- the keychain secret github_commit_signing_key, never a column.
-- pending_public_key is a rotation's new key, NULL outside one.
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

-- commit_signatures: one row per signature a run's git asked for. It is the
-- signing log, which the signing budgets count, so no counter is stored, and
-- which a push's audit row reads for the account that signed its tip.
-- conversation_id and claim_id null on the conversation's purge, so the log
-- outlives it.
--
-- A local daemon signs before it records, so its rows are inserted 'signed',
-- with the commit id the signature produced. The other states ('requested',
-- 'signing', 'refused') and the columns only the control plane writes
-- (claimed_by, and a refusal's retry_after_seconds, refusal_limit,
-- signer_status and signer_status_reason) are multi mode's queue, and stay
-- empty here.
CREATE TABLE commit_signatures (
    id                   TEXT PRIMARY KEY,
    org_id               TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    conversation_id      TEXT REFERENCES conversations(id) ON DELETE SET NULL,
    claim_id             TEXT REFERENCES claims(id) ON DELETE SET NULL,
    boot_epoch           INTEGER NOT NULL,
    signer               TEXT NOT NULL,
    signer_host          TEXT NOT NULL CHECK (signer_host <> ''),
    signer_user_id       INTEGER NOT NULL,
    public_key           TEXT,
    digest               TEXT NOT NULL,
    state                TEXT NOT NULL,
    refusal_reason       TEXT,
    retry_after_seconds  INTEGER,
    refusal_limit        INTEGER,
    signer_status        TEXT,
    signer_status_reason TEXT,
    signature            TEXT,
    claimed_by           TEXT,
    signed_at            DATETIME,
    commit_sha           TEXT,
    repository_id        TEXT REFERENCES repositories(id) ON DELETE SET NULL,
    pushed_at            DATETIME,
    verification_reason  TEXT,
    checked_at           DATETIME,
    created_at           DATETIME NOT NULL
);

-- The two budgets count signed rows: an org's in the last minute, and one
-- claim's in total. A push's audit row looks up the signature whose
-- commit_sha is the tip it pushed.
CREATE INDEX commit_signatures_org_signed_idx ON commit_signatures (org_id, signed_at) WHERE state = 'signed';
CREATE INDEX commit_signatures_claim_signed_idx ON commit_signatures (claim_id) WHERE state = 'signed';
CREATE INDEX commit_signatures_commit_idx ON commit_signatures (org_id, commit_sha) WHERE commit_sha IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
