-- +goose Up
-- Stored configuration stops speaking the three Claude Code tier words and
-- carries concrete model ids instead. The words were a CLI alias vocabulary:
-- Claude Code resolves them, the raw Messages API the native loop uses does
-- not, and the pricing datasheet has no row for them — so a value stored as
-- "sonnet" reached one provider path unresolved and every message it produced
-- persisted unpriced. One rewrite, and the vocabulary is gone; nothing
-- translates at dispatch afterward.
--
-- The mapping is the one the dispatch boundary pinned: haiku takes the dated
-- id (aligned with the system jobs' pin), sonnet and opus the undated current
-- ids, which is what a user choosing those words meant.
--
-- Values already concrete, and empty values, are untouched — the WHERE clauses
-- match the three words exactly. conversations.model and the per-message model
-- stamps are deliberately NOT rewritten: those record what actually ran, and a
-- historical row re-labelled with today's id would make the transcript and the
-- ledger lie about a run that already happened.

-- Per-prompt overrides. A user's pin is their choice and is carried across;
-- a shipped seed's is not a choice anyone made — the seeds now select no model
-- at all, so a system-source row lands unset and inherits the team default.
UPDATE prompts SET model = 'claude-haiku-4-5-20251001'
 WHERE model = 'haiku' AND source <> 'system';
UPDATE prompts SET model = 'claude-sonnet-5'
 WHERE model = 'sonnet' AND source <> 'system';
UPDATE prompts SET model = 'claude-opus-5'
 WHERE model = 'opus' AND source <> 'system';
UPDATE prompts SET model = ''
 WHERE model IN ('haiku', 'sonnet', 'opus') AND source = 'system';

-- The team default, and the column default a partial write (the daily-cost-cap
-- surgery) materializes a fresh row from. SQLite has no ALTER COLUMN ... SET
-- DEFAULT, so the DEFAULT half is a table rebuild — existing rows keep their
-- value (the SELECT copies it, after the UPDATE above has mapped it), and
-- team_settings has no indexes, no triggers, and nothing FK-referencing it, so
-- create-copy-drop-rename is self-contained.
UPDATE team_settings SET default_model = 'claude-haiku-4-5-20251001' WHERE default_model = 'haiku';
UPDATE team_settings SET default_model = 'claude-sonnet-5' WHERE default_model = 'sonnet';
UPDATE team_settings SET default_model = 'claude-opus-5' WHERE default_model = 'opus';

CREATE TABLE team_settings_new (
    team_id                            TEXT PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    jira_projects                      TEXT NOT NULL DEFAULT '[]',
    ai_reprioritize_threshold          INTEGER NOT NULL DEFAULT 5,
    ai_preference_update_interval      INTEGER NOT NULL DEFAULT 20,
    default_model                      TEXT NOT NULL DEFAULT 'claude-sonnet-5',
    auto_delegate_enabled              INTEGER NOT NULL DEFAULT 1,
    updated_at                         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    permission_absent_grace_ms         INTEGER NOT NULL DEFAULT 15000,
    permission_absent_autodeny_enabled INTEGER NOT NULL DEFAULT 1,
    max_daily_cost_usd                 REAL,
    branch_template                    TEXT NOT NULL DEFAULT 'tfac/<ticket-id>',
    review_posture                     TEXT NOT NULL DEFAULT 'identity',
    base_branch_push_policy            TEXT NOT NULL DEFAULT 'never',
    auto_mode_enabled                  INTEGER NOT NULL DEFAULT 1
);

INSERT INTO team_settings_new (
    team_id, jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
    default_model, auto_delegate_enabled, updated_at, permission_absent_grace_ms,
    permission_absent_autodeny_enabled, max_daily_cost_usd, branch_template,
    review_posture, base_branch_push_policy, auto_mode_enabled
)
SELECT
    team_id, jira_projects, ai_reprioritize_threshold, ai_preference_update_interval,
    default_model, auto_delegate_enabled, updated_at, permission_absent_grace_ms,
    permission_absent_autodeny_enabled, max_daily_cost_usd, branch_template,
    review_posture, base_branch_push_policy, auto_mode_enabled
FROM team_settings;

DROP TABLE team_settings;

ALTER TABLE team_settings_new RENAME TO team_settings;

-- +goose Down
SELECT 'down not supported';
