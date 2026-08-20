-- +goose Up
-- Claude Code SDK permission posture. Local-mode SDK runs honor this team
-- setting; multi-mode SDK runs always use auto, and native runs ignore it.
-- DEFAULT 1 both seeds future teams and silently opts every existing local
-- installation into auto mode when SQLite materializes the new NOT NULL column.
ALTER TABLE team_settings ADD COLUMN auto_mode_enabled INTEGER NOT NULL DEFAULT 1;

-- +goose Down
SELECT 'down not supported';
