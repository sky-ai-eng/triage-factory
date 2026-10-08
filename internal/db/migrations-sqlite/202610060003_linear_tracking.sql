-- +goose Up
-- Linear as a tracked source: the team ↔ Linear-team tracking rules, the
-- workspace the org's Linear credential belongs to, and the display order of a
-- team's tracked Linear teams.

-- The Linear workspace the org's credential belongs to, learned from the
-- credential's own `organization { id urlKey }` rather than typed by anyone.
-- Written by the credential bind / install and cleared by the unbind; the
-- settings PATCH has no field for either. NULL is "no Linear credential bound".
-- A user's own Linear credential is keyed under linear_workspace_id.
--
-- The Linear poll cadence is not a column here: it is org_event_sources'
-- poll_interval under kind 'linear', the same place GitHub's and Jira's live.
ALTER TABLE org_settings ADD COLUMN linear_workspace_id TEXT;
ALTER TABLE org_settings ADD COLUMN linear_workspace_url_key TEXT;

-- JSON array of the Linear team UUIDs this team tracks, in the order the
-- settings UI shows them. Display order only: linear_team_rules is the source
-- of truth for which Linear teams a team tracks, exactly as
-- jira_project_status_rules is for jira_projects.
ALTER TABLE team_settings ADD COLUMN linear_teams TEXT NOT NULL DEFAULT '[]';

-- One row per (team, Linear team) the team watches. A Linear team is the unit
-- a TF team tracks, the analogue of a Jira project, and it is keyed by its
-- UUID: the key ("ENG") is a display value Linear lets an admin change, so it
-- rides along and is refreshed on every write.
--
-- The rule columns hold workflow-state refs, {"id","name","type"} objects —
-- the members as a JSON array, each canonical as one object. The id is the
-- identity and survives a rename; the name and type are a snapshot from the
-- last save. There is no in_review rule.
--
-- Every members column is a JSON array and every canonical a JSON object or
-- NULL; json_array_length reads a non-array as 0, so without the shape checks
-- a '{}' or 'null' would pass for an empty rule.
--
-- A row is either watched-but-unarmed (every rule empty) or armed (pickup
-- members, and members plus a canonical for in_progress and done). Half a
-- mapping is refused: a rule set that can discover tickets but has nowhere to
-- move them, or the reverse, is not a configuration anything can act on. The
-- "canonical is one of its rule's members" check stays in the HTTP handler,
-- because a CHECK cannot subquery.
--
-- linear_workspace_id is the workspace the Linear team belongs to. A row saved
-- under one workspace names team and state ids no other workspace has, so every
-- read is confined to the org's current workspace, and binding another one
-- leaves these rows stored rather than deleting them. Linear team ids are
-- globally unique, so the workspace stays out of the primary key.
CREATE TABLE linear_team_rules (
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
    CONSTRAINT ltr_armed_or_unarmed CHECK (
        (json_array_length(pickup_members) = 0
            AND json_array_length(in_progress_members) = 0 AND in_progress_canonical IS NULL
            AND json_array_length(done_members) = 0 AND done_canonical IS NULL)
        OR (json_array_length(pickup_members) > 0
            AND json_array_length(in_progress_members) > 0 AND in_progress_canonical IS NOT NULL
            AND json_array_length(done_members) > 0 AND done_canonical IS NOT NULL)
    )
);

-- +goose Down
SELECT 'down not supported';
