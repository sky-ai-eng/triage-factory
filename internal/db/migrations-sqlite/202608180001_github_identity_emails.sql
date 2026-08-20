-- +goose Up

-- PAT-backed automation commits must use the credential owner's verified
-- primary email instead of fabricating a noreply address. User identity rows
-- keep the same attribute so every GitHub identity capture has one shape.
ALTER TABLE agents ADD COLUMN github_org_email TEXT;
ALTER TABLE user_github_identities ADD COLUMN email TEXT;

-- +goose Down
SELECT 'down not supported';
