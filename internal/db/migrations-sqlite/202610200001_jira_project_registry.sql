-- +goose Up
-- Jira projects get a registry keyed by Jira's own project id, and a team's
-- tracked set references a registry row instead of a project key.
--
-- A project key is a display value: Jira lets an admin rename it, and the
-- project keeps its id. While the tracked set held keys, a rename left every
-- rule naming a key no issue carried any more. Now jira_project_status_rules
-- and team_settings.jira_projects hold the registry row's id, and a rename
-- changes only the row's project_key.
--
-- A registry row is one project on one Jira site. external_id is Jira's
-- numeric project id, unique per (org, site); a row this migration could not
-- find one for keeps it NULL until the poll resolves it by its key. Nothing
-- references jira_project_status_rules, so it is rebuilt inside goose's
-- transaction with no foreign keys to toggle.

-- ---------------------------------------------------------------------------
-- 1. The registry
-- ---------------------------------------------------------------------------

CREATE TABLE jira_projects (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    site         TEXT NOT NULL,
    external_id  TEXT,
    project_key  TEXT NOT NULL,
    project_name TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT jira_projects_site_populated CHECK (site <> ''),
    CONSTRAINT jira_projects_key_populated CHECK (project_key <> ''),
    CONSTRAINT jira_projects_external_id_populated CHECK (external_id IS NULL OR external_id <> '')
);

-- The composite-FK target for the rules below.
CREATE UNIQUE INDEX jira_projects_id_org_idx ON jira_projects (id, org_id);
-- A project's identity, per site: Jira's id once it is known, and the key of a
-- row not yet resolved, so a second unresolved row for one key cannot be made.
CREATE UNIQUE INDEX jira_projects_external_identity
    ON jira_projects (org_id, site, external_id) WHERE external_id IS NOT NULL;
CREATE UNIQUE INDEX jira_projects_unresolved_key
    ON jira_projects (org_id, site, project_key) WHERE external_id IS NULL;

-- jira_site is the site each org's rules are registered under:
-- domain.EntityScope("jira", settings), the org's Jira base URL in the
-- ghbase.CanonicalBaseURL form when it is an http(s) URL with an authority.
-- The canonical form is computed here the way 202610050001 computes it, since
-- a base URL saved after that migration is stored as typed.
--
-- An org with no usable base URL takes the site its Jira issues were last
-- polled from, which is where its rules last applied. An org with neither
-- never polled Jira with these rules, and its rules are not carried: a
-- registry row cannot be keyed without a site.
CREATE TEMP TABLE jira_site (
    org_id TEXT PRIMARY KEY,
    site   TEXT NOT NULL
);

INSERT INTO jira_site (org_id, site)
WITH trimmed(org_id, t) AS (
    SELECT org_id, rtrim(trim(base_url, char(9, 10, 11, 12, 13, 32)), '/')
      FROM org_event_sources
     WHERE kind = 'jira' AND base_url IS NOT NULL
),
split(org_id, t, cut) AS (
    SELECT org_id, t, CASE
        WHEN instr(t, '://') = 0 THEN 0
        WHEN instr(substr(t, instr(t, '://') + 3), '/') = 0 THEN length(t)
        ELSE instr(t, '://') + 1 + instr(substr(t, instr(t, '://') + 3), '/')
    END
      FROM trimmed
),
configured(org_id, site) AS (
    SELECT org_id, lower(substr(t, 1, cut)) || substr(t, cut + 1)
      FROM split
     WHERE t <> ''
),
usable(org_id, site) AS (
    SELECT org_id, site FROM configured
     WHERE site GLOB 'http://[^/]*' OR site GLOB 'https://[^/]*'
),
resolved(org_id, site) AS (
    SELECT t.org_id, COALESCE(
        (SELECT u.site FROM usable u WHERE u.org_id = t.org_id),
        (SELECT e.scope FROM entities e
          WHERE e.org_id = t.org_id AND e.source = 'jira' AND e.scope <> ''
          ORDER BY e.last_polled_at DESC, e.id
          LIMIT 1))
      FROM (SELECT DISTINCT org_id FROM teams) t
)
SELECT org_id, site FROM resolved WHERE site IS NOT NULL;

-- jira_seed is one row per project some team's rules name, with the project id
-- the org's Jira issues under that key carry in their snapshots. A snapshot
-- records the project the issue was in when it was read, so the most recently
-- polled issue under the key is the evidence taken.
CREATE TEMP TABLE jira_seed (
    org_id      TEXT NOT NULL,
    site        TEXT NOT NULL,
    project_key TEXT NOT NULL,
    external_id TEXT,
    polled_at   TIMESTAMP,
    PRIMARY KEY (org_id, site, project_key)
);

INSERT INTO jira_seed (org_id, site, project_key)
SELECT DISTINCT t.org_id, s.site, r.project_key
  FROM jira_project_status_rules r
  JOIN teams t ON t.id = r.team_id
  JOIN jira_site s ON s.org_id = t.org_id
 WHERE r.project_key <> '';

-- json_extract raises on malformed JSON, and a WHERE clause does not promise
-- to test json_valid first, so the CASE guards it.
CREATE TEMP TABLE jira_evidence AS
SELECT e.org_id, e.scope AS site,
       substr(e.source_id, 1, instr(e.source_id, '-') - 1) AS project_key,
       CASE WHEN json_valid(e.snapshot_json)
            THEN json_extract(e.snapshot_json, '$.project_id') END AS external_id,
       e.last_polled_at AS polled_at, e.id AS entity_id
  FROM entities e
 WHERE e.source = 'jira' AND instr(e.source_id, '-') > 1;

DELETE FROM jira_evidence WHERE COALESCE(CAST(external_id AS TEXT), '') = '';

UPDATE jira_seed
   SET (external_id, polled_at) = (
       SELECT CAST(v.external_id AS TEXT), v.polled_at
         FROM jira_evidence v
        WHERE v.org_id = jira_seed.org_id AND v.site = jira_seed.site
          AND v.project_key = jira_seed.project_key
        ORDER BY v.polled_at DESC, v.entity_id
        LIMIT 1);

-- Two keys can carry the same project id: a team that tracked a project under
-- its old key and re-added it under the new one after a rename. The id goes to
-- the key with the most recent evidence, which is the project's key now; the
-- other row stays unresolved, and the poll merges it into this one when it
-- resolves its key.
--
-- randomblob()/random() are evaluated once per row, so each row gets its own
-- id; the shape is a v4 uuid, as everywhere else in this schema.
INSERT INTO jira_projects (id, org_id, site, external_id, project_key)
SELECT
    lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
    substr(lower(hex(randomblob(2))), 2) || '-' ||
    substr('89ab', abs(random()) % 4 + 1, 1) ||
    substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
    s.org_id, s.site,
    CASE WHEN s.external_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM jira_seed o
         WHERE o.org_id = s.org_id AND o.site = s.site
           AND o.external_id = s.external_id AND o.project_key <> s.project_key
           AND (o.polled_at > s.polled_at
                OR (o.polled_at = s.polled_at AND o.project_key < s.project_key)))
    THEN s.external_id END,
    s.project_key
  FROM jira_seed s;

-- ---------------------------------------------------------------------------
-- 2. jira_project_status_rules: (team_id, project_key) -> (team_id, jira_project_id)
-- ---------------------------------------------------------------------------
--
-- org_id comes from the team, so both references are composite foreign keys
-- and a team in one org cannot track another org's project row. A rule is a
-- statement about a project, so it cascades with the registry row; untracking
-- deletes the rule and leaves the registry row standing.

CREATE TABLE jira_project_status_rules_new (
    team_id               TEXT NOT NULL,
    jira_project_id       TEXT NOT NULL,
    org_id                TEXT NOT NULL,
    pickup_members        TEXT NOT NULL DEFAULT '[]',
    in_progress_members   TEXT NOT NULL DEFAULT '[]',
    in_progress_canonical TEXT,
    in_review_members     TEXT NOT NULL DEFAULT '[]',
    in_review_canonical   TEXT,
    done_members          TEXT NOT NULL DEFAULT '[]',
    done_canonical        TEXT,
    updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (team_id, jira_project_id),
    FOREIGN KEY (team_id, org_id) REFERENCES teams(id, org_id) ON DELETE CASCADE,
    FOREIGN KEY (jira_project_id, org_id) REFERENCES jira_projects(id, org_id) ON DELETE CASCADE,
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

INSERT INTO jira_project_status_rules_new (
    team_id, jira_project_id, org_id,
    pickup_members, in_progress_members, in_progress_canonical,
    in_review_members, in_review_canonical, done_members, done_canonical, updated_at
)
SELECT r.team_id, p.id, t.org_id,
       r.pickup_members, r.in_progress_members, r.in_progress_canonical,
       r.in_review_members, r.in_review_canonical, r.done_members, r.done_canonical, r.updated_at
  FROM jira_project_status_rules r
  JOIN teams t ON t.id = r.team_id
  JOIN jira_site s ON s.org_id = t.org_id
  JOIN jira_projects p
    ON p.org_id = t.org_id AND p.site = s.site AND p.project_key = r.project_key;

DROP TABLE jira_project_status_rules;
ALTER TABLE jira_project_status_rules_new RENAME TO jira_project_status_rules;

CREATE INDEX jira_project_status_rules_project_idx
    ON jira_project_status_rules (jira_project_id, team_id);

-- ---------------------------------------------------------------------------
-- 3. team_settings.jira_projects: keys -> registry row ids, order kept
-- ---------------------------------------------------------------------------
--
-- A key with no registry row has no rules either (every rule's key got a row
-- above), and the display order names only tracked projects, so it is dropped.

UPDATE team_settings
   SET jira_projects = COALESCE((
       SELECT json_group_array(p.id ORDER BY CAST(j.key AS INTEGER))
         FROM json_each(team_settings.jira_projects) j
         JOIN teams t ON t.id = team_settings.team_id
         JOIN jira_site s ON s.org_id = t.org_id
         JOIN jira_projects p
           ON p.org_id = t.org_id AND p.site = s.site AND p.project_key = j.value), '[]')
 WHERE json_valid(jira_projects);

DROP TABLE jira_evidence;
DROP TABLE jira_seed;
DROP TABLE jira_site;

-- +goose Down
SELECT 'down not supported';
