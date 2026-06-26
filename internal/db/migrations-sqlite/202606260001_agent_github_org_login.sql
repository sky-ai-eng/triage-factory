-- +goose Up
-- Org credential's own GitHub login (TFAC-452): the bot/org credential's
-- *own* GitHub login was never stored — agents.github_pat_user_id is the
-- user who pasted the PAT, not the login that credential authenticates as.
-- The credential resolver's OrgIdentityFor reads this (PAT tier) to stamp the
-- org identity as the commit author/committer on delegated-agent commits. App
-- tier resolves <slug>[bot] live and needs no column. PAT-path only; nullable,
-- no default — an org bound before this ticket reads back "" and self-heals on
-- the next PAT re-save. Mirrors the Postgres baseline's agents.github_org_login.
ALTER TABLE agents ADD COLUMN github_org_login TEXT;

-- +goose Down
SELECT 'down not supported';
