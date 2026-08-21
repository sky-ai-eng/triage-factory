-- +goose Up
-- Two changes to jira_project_status_rules, one rebuild.
--
-- FIRST: statuses become identified. A rule's members and canonicals now hold
-- {"id","name"} refs rather than bare display names — the members columns as a
-- JSON array of objects, each canonical as a single object. Jira's workflow
-- references the status entity, so the id survives a rename and the name does
-- not: matching on ids is what keeps the discovery JQL and the claim/complete
-- transitions right, and the name rides along so a rule renders without a live
-- fetch. The columns' TYPES are unchanged (they were already TEXT holding
-- JSON), so no data is rewritten here and no row is invalidated: a row written
-- before this carries names and no ids, which is a shape every reader accepts
-- — it matches by name and the ids fill on that rule's next save.
--
-- SECOND: split watched from armed. A jira_project_status_rules row is the team's
-- commitment to WATCH a project; whether that project is ARMED — pickup
-- members plus members-and-canonical on in-progress and done — is now a
-- separate question the row is allowed to answer "not yet" to. Watching is one
-- click in the picker; mapping the workflow's statuses is the step after it,
-- and a row that cannot be stored until both are done is what forced them into
-- one gesture.
--
-- So the three all-or-nothing CHECKs become complete-or-empty, per rule:
--
--   * pickup loses its constraint outright. It has no canonical column, so
--     empty members is the whole of "unset" and there is nothing left to
--     cross-check.
--   * in_progress and done keep members-and-canonical bound together — the
--     canonical is the status TF transitions a ticket INTO, so members without
--     one is a rule that cannot be executed — and additionally admit the state
--     where neither is set.
--
-- The "canonical is in members" check stays in the HTTP validator, as before:
-- a CHECK cannot subquery in either dialect. Stored rows are unaffected — every
-- one of them is complete, which the old CHECKs guaranteed, and complete still
-- satisfies the new ones.
--
-- SQLite can't ALTER a CHECK in place, so the table is rebuilt and the rows
-- copied. Nothing references jira_project_status_rules, so no child FK has to
-- be toggled; the table's own FK to teams(id) is preserved in the new
-- definition.
CREATE TABLE jira_project_status_rules_new (
    team_id               TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project_key           TEXT NOT NULL,
    pickup_members        TEXT NOT NULL DEFAULT '[]',
    in_progress_members   TEXT NOT NULL DEFAULT '[]',
    in_progress_canonical TEXT,
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
