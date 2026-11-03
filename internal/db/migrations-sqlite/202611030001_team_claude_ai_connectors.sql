-- +goose Up
-- Whether a local SDK run on the operator's Claude Code login keeps that
-- login's claude.ai connectors. Off for every existing team and every new one:
-- a run reads untrusted text, and the connectors act on the operator's account.
ALTER TABLE team_settings ADD COLUMN claude_ai_connectors_enabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down
SELECT 'down not supported';
