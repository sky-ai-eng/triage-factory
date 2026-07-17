-- +goose Up
-- Flip team_settings.auto_delegate_enabled's DEFAULT from 0 to 1 so a new team
-- is auto-delegate-on out of the box: a team whose trigger is enabled means the
-- run to fire, and every shipped trigger is off until a human opts in, so a
-- second gate defaulting off just silently swallows the trigger they enabled
-- (a Slack channel claim, for instance, seeds an enabled mention trigger). The
-- Postgres baseline and domain.DefaultTeamSettings already default TRUE; this
-- brings the shipped (frozen) SQLite baseline into line.
--
-- SQLite has no ALTER COLUMN ... SET DEFAULT, so rebuild the table. Existing
-- rows keep their auto_delegate_enabled value (the SELECT copies it) — only the
-- default for FUTURE inserts changes. team_settings has no indexes, no triggers,
-- and nothing FK-references it, so create-copy-drop-rename is self-contained.
CREATE TABLE team_settings_new (
    team_id                            TEXT PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    jira_projects                      TEXT NOT NULL DEFAULT '[]',
    ai_reprioritize_threshold          INTEGER NOT NULL DEFAULT 5,
    ai_preference_update_interval      INTEGER NOT NULL DEFAULT 20,
    default_model                      TEXT NOT NULL DEFAULT 'sonnet',
    auto_delegate_enabled              INTEGER NOT NULL DEFAULT 1,
    updated_at                         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    permission_absent_grace_ms         INTEGER NOT NULL DEFAULT 15000,
    permission_absent_autodeny_enabled INTEGER NOT NULL DEFAULT 1,
    max_daily_cost_usd                 REAL,
    branch_template                    TEXT NOT NULL DEFAULT 'tfac/<ticket-id>'
);

INSERT INTO team_settings_new (
    team_id, jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
    default_model, auto_delegate_enabled, updated_at, permission_absent_grace_ms,
    permission_absent_autodeny_enabled, max_daily_cost_usd, branch_template
)
SELECT
    team_id, jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
    default_model, auto_delegate_enabled, updated_at, permission_absent_grace_ms,
    permission_absent_autodeny_enabled, max_daily_cost_usd, branch_template
FROM team_settings;

DROP TABLE team_settings;

ALTER TABLE team_settings_new RENAME TO team_settings;

-- +goose Down
SELECT 'down not supported';
