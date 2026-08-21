-- +goose Up
-- jira_project_status_rules gains a fourth rule: in_review — the status a team
-- names for work that is awaiting human review.
--
-- It is OPTIONAL, and it is the one rule here that feeds nothing TF polls or
-- classifies on. It is never a classification in particular: no Jira status is
-- read back into TF's in_review board column, which is a fact about a run
-- rather than about a ticket. It does not reach the discovery JQL either, so
-- it is not part of what arms a project, and a team that leaves it empty is
-- completely configured.
--
-- Its CHECK matches the two the other write targets carry: members and
-- canonical together, or neither. That is cross-column, and SQLite cannot add
-- a CHECK in place, so the table is rebuilt and the rows copied. Nothing
-- references jira_project_status_rules, so no child FK has to be toggled; the
-- table's own FK to teams(id) is preserved in the new definition. Existing
-- rows land with the column defaults, which is the empty (unmapped) rule.
CREATE TABLE jira_project_status_rules_new (
    team_id               TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project_key           TEXT NOT NULL,
    pickup_members        TEXT NOT NULL DEFAULT '[]',
    in_progress_members   TEXT NOT NULL DEFAULT '[]',
    in_progress_canonical TEXT,
    in_review_members     TEXT NOT NULL DEFAULT '[]',
    in_review_canonical   TEXT,
    done_members          TEXT NOT NULL DEFAULT '[]',
    done_canonical        TEXT,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, project_key),
    CONSTRAINT jpsr_in_progress_complete_or_empty CHECK (
        (in_progress_members IN ('', '[]')
            AND (in_progress_canonical IS NULL OR in_progress_canonical = ''))
        OR (in_progress_members NOT IN ('', '[]')
            AND in_progress_canonical IS NOT NULL AND in_progress_canonical <> '')
    ),
    CONSTRAINT jpsr_in_review_complete_or_empty CHECK (
        (in_review_members IN ('', '[]')
            AND (in_review_canonical IS NULL OR in_review_canonical = ''))
        OR (in_review_members NOT IN ('', '[]')
            AND in_review_canonical IS NOT NULL AND in_review_canonical <> '')
    ),
    CONSTRAINT jpsr_done_complete_or_empty CHECK (
        (done_members IN ('', '[]')
            AND (done_canonical IS NULL OR done_canonical = ''))
        OR (done_members NOT IN ('', '[]')
            AND done_canonical IS NOT NULL AND done_canonical <> '')
    )
);

INSERT INTO jira_project_status_rules_new
    (team_id, project_key, pickup_members, in_progress_members, in_progress_canonical,
     done_members, done_canonical, updated_at)
SELECT team_id, project_key, pickup_members, in_progress_members, in_progress_canonical,
       done_members, done_canonical, updated_at
FROM jira_project_status_rules;

DROP TABLE jira_project_status_rules;
ALTER TABLE jira_project_status_rules_new RENAME TO jira_project_status_rules;

-- +goose Down
SELECT 'down not supported';
