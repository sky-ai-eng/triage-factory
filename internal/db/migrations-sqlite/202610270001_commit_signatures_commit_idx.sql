-- +goose Up
-- A push's audit row names the account that signed the pushed tip, read from
-- the signature whose commit_sha is that tip.
CREATE INDEX commit_signatures_commit_idx ON commit_signatures (org_id, commit_sha) WHERE commit_sha IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
