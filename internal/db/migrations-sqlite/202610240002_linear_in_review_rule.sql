-- +goose Up
-- linear_team_rules gains the optional fourth rule jira_project_status_rules
-- has: in_review, the state a team names for work awaiting human review.
--
-- The poller and the claim path read none of it, as on Jira, so it is not part
-- of what arms a row, and a team that leaves it empty is completely
-- configured. A state may sit in both in_progress_members and
-- in_review_members.
--
-- Its CHECKs match the other write targets': members are a JSON array, the
-- canonical a JSON object, and the two are set together or not at all. Those
-- are cross-column, and SQLite cannot add a CHECK in place, so the table is
-- rebuilt and the rows copied. Nothing references linear_team_rules, so no
-- child foreign key has to be toggled; its own foreign key to teams(id) is
-- kept in the new definition. Existing rows land with the column defaults,
-- which is the empty rule.
CREATE TABLE linear_team_rules_new (
    team_id               TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    linear_workspace_id   TEXT NOT NULL,
    linear_team_id        TEXT NOT NULL,
    linear_team_key       TEXT NOT NULL,
    linear_team_name      TEXT NOT NULL DEFAULT '',
    pickup_members        TEXT NOT NULL DEFAULT '[]',
    in_progress_members   TEXT NOT NULL DEFAULT '[]',
    in_progress_canonical TEXT,
    in_review_members     TEXT NOT NULL DEFAULT '[]',
    in_review_canonical   TEXT,
    done_members          TEXT NOT NULL DEFAULT '[]',
    done_canonical        TEXT,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, linear_team_id),
    CONSTRAINT ltr_linear_team_id_populated CHECK (linear_team_id <> ''),
    CONSTRAINT ltr_linear_team_key_populated CHECK (linear_team_key <> ''),
    CONSTRAINT ltr_members_are_arrays CHECK (
        json_valid(pickup_members) AND json_type(pickup_members) = 'array'
        AND json_valid(in_progress_members) AND json_type(in_progress_members) = 'array'
        AND json_valid(in_review_members) AND json_type(in_review_members) = 'array'
        AND json_valid(done_members) AND json_type(done_members) = 'array'
    ),
    CONSTRAINT ltr_canonicals_are_objects CHECK (
        (in_progress_canonical IS NULL
            OR (json_valid(in_progress_canonical) AND json_type(in_progress_canonical) = 'object'))
        AND (in_review_canonical IS NULL
            OR (json_valid(in_review_canonical) AND json_type(in_review_canonical) = 'object'))
        AND (done_canonical IS NULL
            OR (json_valid(done_canonical) AND json_type(done_canonical) = 'object'))
    ),
    CONSTRAINT ltr_in_progress_complete_or_empty CHECK (
        (json_array_length(in_progress_members) = 0 AND in_progress_canonical IS NULL)
        OR (json_array_length(in_progress_members) > 0 AND in_progress_canonical IS NOT NULL)
    ),
    CONSTRAINT ltr_in_review_complete_or_empty CHECK (
        (json_array_length(in_review_members) = 0 AND in_review_canonical IS NULL)
        OR (json_array_length(in_review_members) > 0 AND in_review_canonical IS NOT NULL)
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
