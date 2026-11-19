-- +goose Up
-- The Claude Code plugin directories a team loads into its local delegated
-- runs, as a JSON list of {path, required, env, grant_tools}. Empty for every
-- existing team and every new one, which leaves a run's spawn unchanged.
ALTER TABLE team_settings ADD COLUMN local_plugins TEXT NOT NULL DEFAULT '[]';

-- What one engagement's wrapper reported about those plugins, as JSON. A
-- resume is a new claim, so each segment of a run records its own. NULL when
-- the engagement was given no plugin or never reached Claude Code's init.
ALTER TABLE claims ADD COLUMN plugins_loaded TEXT;

-- +goose Down
SELECT 'down not supported';
