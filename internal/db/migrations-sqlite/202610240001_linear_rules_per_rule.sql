-- +goose Up
-- linear_team_rules holds each rule complete or empty on its own, as
-- jira_project_status_rules does. The table-wide CHECK admitted a row with
-- every rule empty or every rule complete and nothing between, so a mapping
-- could only be saved whole; the editor maps a team a rule at a time and saves
-- as each rule lands.
--
--   * pickup has no constraint. It has no canonical column, so empty members
--     is the whole of "unset" and there is nothing to cross-check.
--   * in_progress and done keep members and canonical bound together — the
--     canonical is the state TF moves an issue INTO, so members without one is
--     a rule that cannot be executed — and also admit neither being set.
--
-- A row with some rules mapped is watched and unarmed: the poller asks nothing
-- of a team until all three are mapped. "Canonical is in members" stays in the
-- HTTP validator, since a CHECK cannot subquery. Every stored row satisfies the
-- new CHECKs, because empty and complete rules are both still admitted.
--
-- SQLite can't drop a CHECK in place, so the table is rebuilt and the rows
-- copied. Nothing references linear_team_rules, so no child foreign key has to
-- be toggled; its own foreign key to teams(id) is kept in the new definition.
CREATE TABLE linear_team_rules_new (
    team_id               TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    linear_workspace_id   TEXT NOT NULL,
    linear_team_id        TEXT NOT NULL,
    linear_team_key       TEXT NOT NULL,
    linear_team_name      TEXT NOT NULL DEFAULT '',
    pickup_members        TEXT NOT NULL DEFAULT '[]',
    in_progress_members   TEXT NOT NULL DEFAULT '[]',
    in_progress_canonical TEXT,
    done_members          TEXT NOT NULL DEFAULT '[]',
    done_canonical        TEXT,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, linear_team_id),
    CONSTRAINT ltr_linear_team_id_populated CHECK (linear_team_id <> ''),
    CONSTRAINT ltr_linear_team_key_populated CHECK (linear_team_key <> ''),
    CONSTRAINT ltr_members_are_arrays CHECK (
        json_valid(pickup_members) AND json_type(pickup_members) = 'array'
        AND json_valid(in_progress_members) AND json_type(in_progress_members) = 'array'
        AND json_valid(done_members) AND json_type(done_members) = 'array'
    ),
    CONSTRAINT ltr_canonicals_are_objects CHECK (
        (in_progress_canonical IS NULL
            OR (json_valid(in_progress_canonical) AND json_type(in_progress_canonical) = 'object'))
        AND (done_canonical IS NULL
            OR (json_valid(done_canonical) AND json_type(done_canonical) = 'object'))
    ),
    CONSTRAINT ltr_in_progress_complete_or_empty CHECK (
        (json_array_length(in_progress_members) = 0 AND in_progress_canonical IS NULL)
        OR (json_array_length(in_progress_members) > 0 AND in_progress_canonical IS NOT NULL)
    ),
    CONSTRAINT ltr_done_complete_or_empty CHECK (
        (json_array_length(done_members) = 0 AND done_canonical IS NULL)
        OR (json_array_length(done_members) > 0 AND done_canonical IS NOT NULL)
    )
);

INSERT INTO linear_team_rules_new
    (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name,
     pickup_members, in_progress_members, in_progress_canonical,
     done_members, done_canonical, updated_at)
SELECT team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name,
       pickup_members, in_progress_members, in_progress_canonical,
       done_members, done_canonical, updated_at
FROM linear_team_rules;

DROP TABLE linear_team_rules;
ALTER TABLE linear_team_rules_new RENAME TO linear_team_rules;

-- +goose Down
SELECT 'down not supported';
