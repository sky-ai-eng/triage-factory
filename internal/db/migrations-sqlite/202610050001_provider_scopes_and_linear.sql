-- +goose NO TRANSACTION
-- +goose Up
-- Every external object is keyed within the provider namespace it lives in, a
-- GitHub entity references its repository's registry row and records its pull
-- request's node id, a repository row can be retired to give up a name GitHub
-- has handed to another repository, a GitHub team mapping records GitHub's
-- ids for the team and its organization, a team's Jira tracked set and every
-- Jira issue entity reference a Jira project registry row, every Jira and Linear
-- issue entity is written with its issue id, pull request, review and branch
-- artifacts are keyed on ids rather than their repository's name, every
-- artifact references the entity it is about and Jira artifacts keyed on an
-- issue key are keyed on the issue's id, GitHub event-handler predicates name
-- people and teams by GitHub's id, Linear
-- becomes a tracked source, and six smaller changes ride along: the org GitHub App's unusable state, the claim's source
-- manifest, the retirement of the 'completed' conversation status, node ids on
-- artifacts and audit-ledger rows, the table of rate-limit readings, and the
-- indexes that look a GitHub account up by GitHub's id (an identity binding,
-- and a pull request's author).
--
-- NO TRANSACTION because rebuilding a table other tables reference needs
-- foreign keys off, and PRAGMA foreign_keys is a no-op inside a transaction:
-- with them on, dropping the parent runs an implicit DELETE that cascades into
-- every row referencing it. The pragma is set first and everything else runs
-- in one explicit transaction, so a failure part way commits nothing and the
-- next boot retries the whole file. The pool is capped at one connection, so
-- the pragma and the transaction hold for every statement here. Nothing the
-- DML below deletes is referenced by another table, so no cascade is skipped.

PRAGMA foreign_keys = off;

BEGIN IMMEDIATE;

-- ===========================================================================
-- 1. Smaller changes
-- ===========================================================================

-- Whether GitHub still accepts the org's own GitHub App. An App can be deleted
-- on GitHub, or have its private key deleted or regenerated there, without TF
-- being told: no webhook announces either, and both look like any other failed
-- request from inside a poll cycle. The installation reconcile asks GitHub
-- directly (GET /app with the App's JWT) when its listing is refused, and
-- records the answer here so the Settings panel, the poller and credential
-- resolution all read the same fact.
--
-- unusable_reason is NULL while GitHub accepts the App, else 'missing' (no App
-- with this id exists) or 'key_rejected' (the stored key no longer signs for
-- it). unusable_since is when the current reason was first observed, NULL
-- whenever the reason is. App-validated, not CHECK-constrained, matching the
-- other ADD COLUMN text columns: widening a SQLite CHECK means a full table
-- rebuild.
ALTER TABLE org_github_apps ADD COLUMN unusable_reason TEXT;
ALTER TABLE org_github_apps ADD COLUMN unusable_since DATETIME;

-- claims.source_manifest: an engagement's source manifest — for each event
-- source, whether its credential was sealed into the engagement's bundle and,
-- when not, why not. Multi mode's credential provisioner writes it on every
-- seal. Local mode seals no bundles, so nothing writes it here; the column
-- keeps the claims projection both dialects read one shape. NULL = never
-- sealed.
ALTER TABLE claims ADD COLUMN source_manifest TEXT;

-- A conversation does not conclude. Its status says whether the transcript can
-- be driven (queued, running, open, or failed when the runtime under it died),
-- and whether the work is done is the blueprint run's to say. A step's verdict
-- (outcome, outcome_reason, result_summary) stays on the step's conversation,
-- because the conversation is the step instance, and recording it parks the
-- row `open` like any other turn end.
--
-- So 'completed' is not in the stored vocabulary, and the rows carrying it are
-- rewritten rather than taught to every predicate: those predicates are
-- exclusions (`status NOT IN (…)`), and a value left out of one readmits a
-- concluded conversation instead of failing closed. Every row this touches
-- keeps its verdict columns, and its blueprint run already holds the terminal
-- the verdict produced (or has advanced past the step), so `open` with a
-- verdict under a blueprint that no longer drives it is the faithful spelling
-- of what happened. Rewritten rows are indistinguishable from a verdict
-- recorded by this build.
--
-- completed_at keeps the terminal write's stamp: it is the conclusion stamp
-- that makes an `open` row concluded, and what the idle sweeps age the row
-- from. A row the terminal write left without one is stamped from the oldest
-- evidence it carries (its boundary, then its start), because an `open` row
-- with no stamp is unsettled: it would read as live and be claimable on
-- undelivered input. park_reason is cleared: a verdict records no reason,
-- because nothing stopped the conversation, and a reason left over from an
-- earlier park would describe a turn that is no longer the last one.
UPDATE conversations
SET status = 'open',
    completed_at = COALESCE(completed_at, ended_at, started_at),
    park_reason = NULL
WHERE status = 'completed';

-- The latest reading of every rate-limit bucket the org's credentials are
-- metered on, keyed (upstream, credential, bucket) within the org. The process
-- making requests keeps its readings in memory and writes the ones that changed
-- here, and GET /api/orgs/{org_id}/integrations/rate-limits reads them; an
-- upsert keeps a key's newest observation. credential is the opaque id the
-- upstream's client names it by, never a token or a login. A NULL column is a
-- value the reading did not state, which is not the same as zero; reset_at
-- NULL means the reading named no reset, and such a reading is reported for an
-- hour after observed_at (domain.RateLimitReadingMaxAge).
CREATE TABLE rate_limit_readings (
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    upstream     TEXT NOT NULL,
    credential   TEXT NOT NULL,
    bucket       TEXT NOT NULL,
    remaining    INTEGER,
    bucket_limit INTEGER,
    used         INTEGER,
    near_limit   BOOLEAN,
    reset_at     DATETIME,
    observed_at  DATETIME NOT NULL,
    PRIMARY KEY (org_id, upstream, credential, bucket)
);

-- The reverse (host, GitHub account id) -> user lookup the routing
-- subscribers run for an event that names its actor by GitHub's id rather
-- than by login (UserIDsForGitHubUserIDSystem). The id-arm twin of
-- user_github_identities_login_lookup_idx.
CREATE INDEX user_github_identities_user_id_lookup_idx
    ON user_github_identities (github_base_url, github_user_id)
    WHERE github_user_id IS NOT NULL;

-- ===========================================================================
-- 2. Linear
-- ===========================================================================

-- user_linear_identities — workspace-scoped Linear identity bindings, the
-- Linear sibling of user_github_identities / user_jira_identities.
--
-- Linear is cloud-only, so the scope a person's identity lives in is not a
-- host but a workspace: one human holds an independent, workspace-scoped User
-- record in every Linear workspace they belong to, and a user id from one
-- workspace means nothing in another. The natural key is therefore (user_id,
-- workspace_id), where workspace_id is the Linear organization id of the
-- workspace the org's Linear credential belongs to — learned from that
-- credential, never typed. The per-user Linear credential is custodied under
-- the same id ("linear_token/<workspace_id>"), so identity and access are
-- keyed alike.
--
-- linear_user_id is the workspace-scoped User UUID (viewer.id), the match key
-- for assignee and author predicates; display_name is viewer.displayName.
-- source records how the binding was captured: 'api_key' (a pasted personal
-- API key — Linear has no PAT), 'connect_oauth' (one-click Connect), or
-- 'scim' (reserved; no writer). The value set is closed because identity
-- provenance is security-relevant. verified_at is the last authenticated
-- viewer confirmation, nullable for a directory sync that learns an identity
-- without one. An absent row is a supported state: the reader degrades
-- exactly as it does for the GitHub and Jira siblings.
CREATE TABLE user_linear_identities (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id   TEXT NOT NULL,
    linear_user_id TEXT NOT NULL,
    display_name   TEXT,
    source         TEXT NOT NULL
                       CHECK (source IN ('api_key', 'connect_oauth', 'scim')),
    verified_at    TIMESTAMP,
    created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, workspace_id)
);

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

-- org_linear_apps — the org's own Linear OAuth app, the Linear sibling of
-- org_jira_apps. The install ceremony and the per-user Connect run against
-- it. The row is the per-org override in the app precedence: an org with no
-- row falls back to the deployment app, or has none. client_secret_ref names
-- the org secret "linear_oauth_client_secret"; the secret never lives here.
CREATE TABLE org_linear_apps (
    org_id                TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    client_id             TEXT NOT NULL,
    client_secret_ref     TEXT NOT NULL,
    registered_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    registered_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL
);

-- org_linear_installs — one row per org: the Linear workspace that installed
-- the org's resolved app (actor=app), whichever app that was. The refresh
-- token lives in the org secret "linear_app_install"; this row is the
-- queryable half.
--
-- install_id is minted per install and stored in the secret too, so a writer
-- holding one names exactly the install it read and cannot remove a newer one.
-- app_user_id is viewer.id under the app token, the identity TF acts as in the
-- workspace. app_client_id is the app that minted the install: a refresh
-- needs that app's secret. installed_by_user_id is a soft reference, so the
-- row outlives the admin who installed it. removed_reason is set exactly when
-- removed_at is.
CREATE TABLE org_linear_installs (
    org_id               TEXT PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    install_id           TEXT NOT NULL CHECK (install_id <> ''),
    workspace_id         TEXT NOT NULL,
    workspace_url_key    TEXT NOT NULL,
    app_user_id          TEXT NOT NULL,
    app_client_id        TEXT NOT NULL,
    installed_by_user_id TEXT,
    installed_at         TIMESTAMP NOT NULL,
    removed_at           TIMESTAMP,
    removed_reason       TEXT
                             CHECK (removed_reason IN ('disconnected', 'install_revoked', 'install_failed')),
    CHECK ((removed_at IS NULL) = (removed_reason IS NULL))
);

-- One Linear workspace installs into at most one TF org. The credential is a
-- per-workspace token, so nothing else can enforce it. A removed install holds
-- nothing, so the workspace can be installed again, here or elsewhere.
CREATE UNIQUE INDEX org_linear_installs_workspace_live
    ON org_linear_installs (workspace_id) WHERE removed_at IS NULL;

-- ===========================================================================
-- 3. Provider scopes
-- ===========================================================================
--
-- A key or provider id is unique only inside the namespace its provider issued
-- it in: owner/repo#18 and repository and installation ids exist again on
-- another GitHub host, and two Jira sites or Linear workspaces repeat issue
-- keys. So every table keying an external object by one carries that
-- namespace, its scope:
--
--   entities                      scope, and the provider's own id beside the
--                                 key (external_id); a GitHub row also
--                                 references its repository's registry row
--                                 (repository_id). The table is rebuilt in
--                                 section 5, once the Jira project registry
--                                 a Jira row references exists.
--   repositories                  host
--   team_github_groups            host, and GitHub's ids for the team and
--                                 its organization (section 3d)
--   org_github_app_installations  github_host joins the primary key
--   reachable_repositories,       host on every row
--     reachable_scopes
--   artifacts, external_actions   scope joins the dedup key
--   placement_overrides           host joins the primary key
--
-- team_github_repos and conversation_worktrees reference a repository by row
-- id, so they take the scope from the row and get no column.
--
-- A host or site is compared by exact string wherever it is a key, so every
-- one is also stored in one spelling: the ghbase.CanonicalBaseURL form, which
-- lowercases the scheme and the authority, keeps the path's case (a GHES or
-- Jira Data Center context path may be case-sensitive), and trims whitespace
-- and trailing slashes. The org's GitHub and Jira base URLs and every column
-- already holding a host or site are rewritten into it, and every scope
-- written below is computed from the rewritten values.
--
-- Every existing row takes the org's current scope for its provider: a local
-- install has only ever had one GitHub, one Jira site and one Linear workspace
-- at a time. migration_provider_scopes_and_linear_test.go pins each value
-- computed here against the Go function every later read and write computes
-- it with.

-- host_case maps every stored spelling of a host or site to its canonical
-- form. SQLite has no URL parser, so the form is computed with string
-- functions, once per distinct value. lower() folds ASCII only, as the Go side
-- does. A value of only whitespace and slashes canonicalizes to nothing and
-- gets no row: no writer stores one, and every lookup below treats it as
-- unset.
CREATE TEMP TABLE host_case (
    raw       TEXT PRIMARY KEY,
    canonical TEXT NOT NULL
);

INSERT INTO host_case (raw, canonical)
WITH spellings(v) AS (
    SELECT base_url FROM org_event_sources WHERE kind IN ('github', 'jira')
    -- A Jira entity's url up to '/browse/' is the site the tracker built it
    -- from.
    UNION SELECT substr(url, 1, instr(url, '/browse/') - 1) FROM entities WHERE source = 'jira'
    UNION SELECT github_base_url FROM user_github_identities
    UNION SELECT jira_base_url FROM user_jira_identities
    UNION SELECT github_host FROM org_github_app_installations
    UNION SELECT host FROM reachable_repositories
    UNION SELECT scope FROM reachable_scopes WHERE credential_class = 'pat'
),
-- strings.TrimRight(strings.TrimSpace(v), "/"), with TrimSpace's ASCII set.
trimmed(v, t) AS (
    SELECT v, rtrim(trim(v, char(9, 10, 11, 12, 13, 32)), '/') FROM spellings WHERE v IS NOT NULL
),
-- cut: the length of the prefix that folds, scheme "://" authority. 0 when
-- the value has no "://" and so no authority to find.
split(v, t, cut) AS (
    SELECT v, t, CASE
        WHEN instr(t, '://') = 0 THEN 0
        WHEN instr(substr(t, instr(t, '://') + 3), '/') = 0 THEN length(t)
        ELSE instr(t, '://') + 1 + instr(substr(t, instr(t, '://') + 3), '/')
    END
      FROM trimmed
)
SELECT v, lower(substr(t, 1, cut)) || substr(t, cut + 1) FROM split WHERE t <> '';

-- org_scope is the scope each org's objects of a provider are keyed under now
-- (domain.ExternalObjectScope, which is domain.EntityScope for a source):
--
--   github, git  the org's GitHub base URL, canonical, and https://github.com
--                when none is set. Go resolves no host for an empty setting;
--                boot gives an org that uses GitHub this URL
--                (integrations.AdoptSavedGitHubHost), and TF_DEFAULT_GITHUB_HOST
--                is a multi-mode setting a migration cannot read anyway.
--   jira         the org's Jira base URL, canonical, when it is an http(s) URL
--                with an authority (domain.JiraHost); empty otherwise, which no
--                read asks for.
--   slack        domain.SlackScope.
--   network      domain.NetworkScope.
--
-- Linear has no row: no org has a Linear workspace bound yet, so its scope is
-- empty, as is that of any provider not listed.
CREATE TEMP TABLE org_scope (
    org_id   TEXT NOT NULL,
    provider TEXT NOT NULL,
    scope    TEXT NOT NULL,
    PRIMARY KEY (org_id, provider)
);

INSERT INTO org_scope (org_id, provider, scope)
WITH org_ids(org_id) AS (
    SELECT id FROM orgs
    UNION SELECT org_id FROM teams
    UNION SELECT org_id FROM entities
    UNION SELECT org_id FROM repositories
    UNION SELECT org_id FROM artifacts
    UNION SELECT org_id FROM external_actions
    UNION SELECT org_id FROM placement_overrides
),
base(org_id, kind, canonical) AS (
    SELECT s.org_id, s.kind, c.canonical
      FROM org_event_sources s
      JOIN host_case c ON c.raw = s.base_url
     WHERE s.kind IN ('github', 'jira')
),
github(org_id, scope) AS (
    SELECT o.org_id, COALESCE(b.canonical, 'https://github.com')
      FROM org_ids o
      LEFT JOIN base b ON b.org_id = o.org_id AND b.kind = 'github'
),
jira(org_id, scope) AS (
    SELECT o.org_id, COALESCE(b.canonical, '')
      FROM org_ids o
      LEFT JOIN base b ON b.org_id = o.org_id AND b.kind = 'jira'
           AND (b.canonical GLOB 'http://[^/]*' OR b.canonical GLOB 'https://[^/]*')
)
SELECT org_id, 'github', scope FROM github
UNION ALL SELECT org_id, 'git', scope FROM github
UNION ALL SELECT org_id, 'jira', scope FROM jira
UNION ALL SELECT org_id, 'slack', 'slack.com' FROM org_ids
UNION ALL SELECT org_id, 'network', 'internet' FROM org_ids;

-- ---------------------------------------------------------------------------
-- 3a. Stored hosts and sites, rewritten into the canonical spelling
-- ---------------------------------------------------------------------------
--
-- Lowercasing can make two rows one key. An identity keeps the most recently
-- updated row for the user and site, and a tie keeps the row already in
-- canonical form; the others are deleted. (The reachable-repo cache settles
-- the same way in 3e.) An installation is referenced by its reach rows, so
-- none is deleted: one whose canonical key another org's live installation
-- already holds keeps its old spelling, which no read asks for.

UPDATE org_event_sources
   SET base_url = (SELECT c.canonical FROM host_case c WHERE c.raw = org_event_sources.base_url)
 WHERE kind IN ('github', 'jira')
   AND base_url IN (SELECT raw FROM host_case WHERE canonical <> raw);

DELETE FROM user_github_identities
 WHERE EXISTS (
       SELECT 1
         FROM user_github_identities o
         JOIN host_case oc ON oc.raw = o.github_base_url
         JOIN host_case rc ON rc.raw = user_github_identities.github_base_url
        WHERE o.user_id = user_github_identities.user_id
          AND o.github_base_url <> user_github_identities.github_base_url
          AND oc.canonical = rc.canonical
          AND (o.updated_at, oc.raw = oc.canonical, o.github_base_url)
            > (user_github_identities.updated_at, rc.raw = rc.canonical, user_github_identities.github_base_url));

UPDATE user_github_identities
   SET github_base_url = (SELECT c.canonical FROM host_case c WHERE c.raw = user_github_identities.github_base_url)
 WHERE github_base_url IN (SELECT raw FROM host_case WHERE canonical <> raw);

DELETE FROM user_jira_identities
 WHERE EXISTS (
       SELECT 1
         FROM user_jira_identities o
         JOIN host_case oc ON oc.raw = o.jira_base_url
         JOIN host_case rc ON rc.raw = user_jira_identities.jira_base_url
        WHERE o.user_id = user_jira_identities.user_id
          AND o.jira_base_url <> user_jira_identities.jira_base_url
          AND oc.canonical = rc.canonical
          AND (o.updated_at, oc.raw = oc.canonical, o.jira_base_url)
            > (user_jira_identities.updated_at, rc.raw = rc.canonical, user_jira_identities.jira_base_url));

UPDATE user_jira_identities
   SET jira_base_url = (SELECT c.canonical FROM host_case c WHERE c.raw = user_jira_identities.jira_base_url)
 WHERE jira_base_url IN (SELECT raw FROM host_case WHERE canonical <> raw);

UPDATE OR IGNORE org_github_app_installations
   SET github_host = (SELECT c.canonical FROM host_case c WHERE c.raw = org_github_app_installations.github_host)
 WHERE github_host IN (SELECT raw FROM host_case WHERE canonical <> raw);

-- ---------------------------------------------------------------------------
-- 3b. repositories.host
-- ---------------------------------------------------------------------------
--
-- host joins both of a repository's keys, its name and its provider id:
-- repository ids are per-deployment sequences, so a new host's repository can
-- carry an old host's id.
--
-- SQLite adds a NOT NULL column only with a default. The store never writes an
-- empty host (it refuses one), so the default only ever reaches the rows the
-- backfill below overwrites.
ALTER TABLE repositories ADD COLUMN host TEXT NOT NULL DEFAULT '';

UPDATE repositories
   SET host = (SELECT s.scope FROM org_scope s
                WHERE s.org_id = repositories.org_id AND s.provider = 'github');

-- retired_at is set when GitHub has given a row's name to another repository
-- and the row's own id no longer answers: the repository was deleted, or the
-- org's credential can no longer see it. A retired row keeps its id, name,
-- provider id and everything that references it, and stops holding the name.
-- So the identity index covers only rows that are not retired, as
-- entities_active_key covers only active entities: the repository now under
-- the name gets a row of its own and never collides with the history.
ALTER TABLE repositories ADD COLUMN retired_at DATETIME;

DROP INDEX repositories_identity;
CREATE UNIQUE INDEX repositories_identity
    ON repositories(org_id, source, host, LOWER(owner), LOWER(repo))
    WHERE retired_at IS NULL;

-- A provider id names one repository on its host. Nothing enforced that
-- before, so two rows may share one; the id stays on the most recently updated
-- of them and the others lose it. A row without an id is a supported state: it
-- is not renamable until it learns one, and the poller and the profiler record
-- ids only where no other row on the host holds them.
UPDATE repositories
   SET external_id = NULL
 WHERE external_id IS NOT NULL
   AND EXISTS (
       SELECT 1 FROM repositories o
        WHERE o.org_id = repositories.org_id
          AND o.source = repositories.source
          AND o.host = repositories.host
          AND o.external_id = repositories.external_id
          AND (COALESCE(o.updated_at, '') > COALESCE(repositories.updated_at, '')
               OR (COALESCE(o.updated_at, '') = COALESCE(repositories.updated_at, '')
                   AND o.id > repositories.id)));

CREATE UNIQUE INDEX repositories_external_identity
    ON repositories(org_id, source, host, external_id)
    WHERE external_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3c. entities.scope, external_id and repository_id
-- ---------------------------------------------------------------------------
--
-- What each existing row is keyed under and references is computed here; the
-- table itself is rebuilt in section 5, after the Jira project registry a Jira
-- row also references (jira_project_id) exists.
--
--   scope        the namespace the entity's key and id are unique within: the
--                GitHub host, the Jira site, the Linear workspace id, or
--                domain.SlackScope.
--   external_id  the provider's id for the object, which a key change does not
--                move: a pull request's GraphQL node id, a Jira issue's numeric
--                id, a Linear issue's UUID. NULL is "not learned yet".
--
-- source_id stays the key everything reads and displays. A key change is a
-- rename of the row that carries the id, rather than a second row.
--
-- Keys are unique among ACTIVE rows of one scope only: a key a provider frees
-- and hands to a new object (a project deleted and recreated, a team key
-- reused) must not collide with the closed history it used to name. That
-- replaces the table-level UNIQUE(source, source_id), so the table is rebuilt.
-- A row's scope is its org's for its source, except that a Jira row takes the
-- site its own url names: that is the site the issue actually came from, even
-- if the org has since been pointed at another one. No new key can collide,
-- because source_id was unique per source.
--
-- entity_scope holds each row's scope. A url with no '/browse/' looks up an
-- empty prefix, which host_case never holds, so the row takes its org's site.
--
-- linear_team_id is the UUID of the Linear team a Linear issue sits in, the id
-- linear_team_rules is keyed by. It is a column rather than a field read out
-- of snapshot_json because turning a source off clears the snapshot, and an
-- issue's team has to survive that: the belt places the issue by it, and an
-- issue that goes unreachable while the source is off names it for the
-- router's team gate. It is set at creation from the response that found the
-- issue and moved by every snapshot write to the snapshot's team, and clearing
-- the snapshot leaves it. Required for a Linear row and NULL for every other
-- source, enforced by the store. No row needs it filled here: no release
-- before this migration polled Linear.
--
-- repository_id is the registry row of the repository a GitHub entity belongs
-- to. The key names the repository by the name it has now, which a rename
-- changes; this does not. The store sets it when it creates the row, from the
-- key, through the registry's get-or-create, so a pull request in a repository
-- no team tracks has a row to reference. Required for a GitHub row, by CHECK.
--
-- Every existing GitHub row's repository is read off its key the way
-- db.GitHubEntityRepoRef reads it — the part before '#' is owner/repo, split at
-- the first '/' — on the host the row is scoped under below, which is the host
-- 3b gave the org's repositories. The SQL cuts at the first '#' and the Go at
-- the last; they agree because a GitHub owner or repository name cannot
-- contain '#', so a key holds one. Every GitHub key a release wrote has that
-- shape: the tracker and the dashboard backfill build "owner/repo#N" from the
-- pull request they polled, and the exec funnel and the memory attach create
-- only a key domain.ParsePRTarget accepts. A repository with no registry row
-- gets a bare one, identity columns only, as the store's get-or-create would
-- mint it; the folded identity index makes the first insert for a repository
-- the one that stands, so the newest key's casing is the one a new row
-- stores. A registry row is not tracking: nothing is added to what TF polls,
-- profiles or clones.
CREATE TEMP TABLE entity_scope (
    entity_id TEXT PRIMARY KEY,
    scope     TEXT NOT NULL
);

INSERT INTO entity_scope (entity_id, scope)
SELECT e.id,
       COALESCE(
           CASE WHEN e.source = 'jira' THEN
               (SELECT c.canonical FROM host_case c
                 WHERE c.raw = substr(e.url, 1, instr(e.url, '/browse/') - 1))
           END,
           s.scope,
           '')
  FROM entities e
  LEFT JOIN org_scope s ON s.org_id = e.org_id AND s.provider = e.source;

CREATE TEMP TABLE github_entity_repo AS
SELECT entity_id, org_id, host, created_at,
       substr(path, 1, instr(path, '/') - 1) AS owner,
       substr(path, instr(path, '/') + 1)    AS repo
  FROM (SELECT e.id AS entity_id, e.org_id, s.scope AS host, e.created_at,
               CASE WHEN instr(e.source_id, '#') > 0
                    THEN substr(e.source_id, 1, instr(e.source_id, '#') - 1)
                    ELSE e.source_id END AS path
          FROM entities e
          JOIN org_scope s ON s.org_id = e.org_id AND s.provider = 'github'
         WHERE e.source = 'github')
 WHERE instr(path, '/') > 1 AND instr(path, '/') < length(path);

INSERT OR IGNORE INTO repositories (id, owner, repo, source, host, org_id, updated_at)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       owner, repo, 'github', host, org_id, CURRENT_TIMESTAMP
  FROM github_entity_repo
 ORDER BY created_at DESC, entity_id;

-- Every existing GitHub row with a snapshot takes its pull request's node id
-- from it: the poller stores node_id in every snapshot it writes. A stub
-- created from a key outside the poller has no snapshot and stays NULL until
-- the poller reads its pull request. One pull request can sit under two rows,
-- created under two names of its repository; the id goes to the older row
-- (earliest created_at, ties broken by id), the one a duplicate merge keeps
-- (db.DuplicateEntityPair), and the other stays NULL. The partition is the
-- scope the row is copied under in section 5. json_extract raises on
-- malformed JSON, so the CASE guards it.
CREATE TEMP TABLE github_entity_node AS
SELECT entity_id, node_id
  FROM (SELECT entity_id, node_id,
               row_number() OVER (PARTITION BY org_id, host, node_id
                                  ORDER BY created_at, entity_id) AS n
          FROM (SELECT e.id AS entity_id, e.org_id, s.scope AS host, e.created_at,
                       CASE WHEN json_valid(e.snapshot_json)
                            THEN json_extract(e.snapshot_json, '$.node_id') END AS node_id
                  FROM entities e
                  JOIN org_scope s ON s.org_id = e.org_id AND s.provider = 'github'
                 WHERE e.source = 'github')
         WHERE COALESCE(CAST(node_id AS TEXT), '') <> '')
 WHERE n = 1;

-- Every existing Jira row whose snapshot names its issue's id takes it, by the
-- same rules: the older of two rows naming one id in a site keeps it, the
-- partition being the scope entity_scope gave the row. A snapshot written
-- before the poller recorded the id (every snapshot v1.15.0 and earlier
-- wrote), a stub with no snapshot and a malformed snapshot leave the row NULL.
-- Such a row learns its id from Jira the first time a read reaches it: the
-- next poll for an active row on the org's site, which reads it by key, and
-- the next read that finds it by key for a closed one
-- (db.EntityStore.StampExternalIDSystem). Closing those rows instead would
-- retire every issue such an install tracks. Section 5's triggers keep any
-- later write from creating a Jira or Linear row without its id.
CREATE TEMP TABLE jira_entity_issue AS
SELECT entity_id, issue_id
  FROM (SELECT entity_id, issue_id,
               row_number() OVER (PARTITION BY org_id, scope, issue_id
                                  ORDER BY created_at, entity_id) AS n
          FROM (SELECT e.id AS entity_id, e.org_id, es.scope, e.created_at,
                       CASE WHEN json_valid(e.snapshot_json)
                            THEN json_extract(e.snapshot_json, '$.id') END AS issue_id
                  FROM entities e
                  JOIN entity_scope es ON es.entity_id = e.id
                 WHERE e.source = 'jira')
         WHERE COALESCE(CAST(issue_id AS TEXT), '') <> '')
 WHERE n = 1;

-- ---------------------------------------------------------------------------
-- 3d. team_github_groups: host, and GitHub's ids for the team
-- ---------------------------------------------------------------------------
--
-- A mapping names a GitHub organization by login, which another host can
-- reuse, and a team by a slug GitHub changes when the team is renamed. So a
-- row records its host, and GitHub's ids for the team (github_team_id) and
-- for its organization (github_org_id), which routing and the deletion
-- reconcile match on; the login and slug are display values. Rebuilt because
-- the key changes.
--
-- The rows copied here carry no ids, since only GitHub can say which team a
-- slug names. The GitHub poll's team reconcile fills them in once, matching
-- the row's org login and slug against the organization's team listing, and
-- never deletes a row it has not identified. Until then routing matches the
-- row by its login and slug. The store refuses a new row without its ids, so
-- a NULL is always a row copied here.
CREATE TABLE team_github_groups_new (
    team_id          TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    host             TEXT NOT NULL,
    github_team_id   TEXT,
    github_org_id    TEXT,
    github_org_login TEXT NOT NULL,
    github_team_slug TEXT NOT NULL,
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (team_id, host, github_team_id),
    CONSTRAINT tgg_host_populated CHECK (host <> ''),
    CONSTRAINT tgg_ids_together CHECK ((github_team_id IS NULL) = (github_org_id IS NULL)),
    CONSTRAINT tgg_team_id_populated CHECK (github_team_id <> ''),
    CONSTRAINT tgg_org_id_populated CHECK (github_org_id <> ''),
    CONSTRAINT tgg_org_login_populated CHECK (github_org_login <> ''),
    CONSTRAINT tgg_team_slug_populated CHECK (github_team_slug <> '')
);

INSERT INTO team_github_groups_new (team_id, host, github_org_login, github_team_slug, created_at)
SELECT g.team_id, s.scope, g.github_org_login, g.github_team_slug, g.created_at
  FROM team_github_groups g
  JOIN teams t ON t.id = g.team_id
  JOIN org_scope s ON s.org_id = t.org_id AND s.provider = 'github';

DROP TABLE team_github_groups;
ALTER TABLE team_github_groups_new RENAME TO team_github_groups;

-- Routing's lookup: which teams a GitHub team's review requests route to. The
-- unique key leads with team_id and cannot serve it.
CREATE INDEX team_github_groups_github_team_idx ON team_github_groups (host, github_team_id);

-- ---------------------------------------------------------------------------
-- 3e. GitHub App installations and the reachable-repo cache
-- ---------------------------------------------------------------------------
--
-- GitHub numbers installations per deployment, so an id names one installation
-- only together with its host, and a login names one account only within one
-- deployment. Three tables change, each rebuilt because SQLite cannot alter a
-- primary key, a CHECK or a foreign key in place:
--
--   org_github_app_installations  github_host joins the primary key and the
--       one-live-install-per-account key. The column has no DEFAULT: a row
--       names the host it was listed on, and nothing stands in for one.
--   reachable_repositories        every row carries its host. An App row's is
--       its installation's, and the foreign key to the installation runs over
--       (org_id, host, installation_id), so a row can only ever hang off the
--       installation on its own host.
--   reachable_scopes              every marker carries its host, the PAT
--       tier's scope already being one. An App marker whose installation no
--       longer exists vouches for nothing and is not copied.
--
-- A PAT row of either cache whose canonical key another row also takes is a
-- repeat of a cached observation: the most recently observed or refreshed row
-- is copied, and a tie copies the one already in canonical form.
CREATE TABLE org_github_app_installations_new (
    installation_id      TEXT NOT NULL,
    org_id               TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    account_type         TEXT NOT NULL CHECK (account_type IN ('Organization','User')),
    account_login        TEXT NOT NULL,
    installed_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    removed_at           TIMESTAMP,
    account_id           TEXT,
    suspended_at         TIMESTAMP,
    suspended_by         TEXT,
    github_host          TEXT NOT NULL CHECK (github_host <> ''),
    repository_selection TEXT
        CHECK (repository_selection IS NULL OR repository_selection IN ('all', 'selected')),
    PRIMARY KEY (org_id, github_host, installation_id)
);

INSERT INTO org_github_app_installations_new
    (installation_id, org_id, account_type, account_login, installed_at, removed_at,
     account_id, suspended_at, suspended_by, github_host, repository_selection)
SELECT installation_id, org_id, account_type, account_login, installed_at, removed_at,
       account_id, suspended_at, suspended_by, github_host, repository_selection
  FROM org_github_app_installations;

CREATE TABLE reachable_repositories_new (
    org_id           TEXT NOT NULL,
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app', 'managed_app')),
    -- installation_id: the installation an App row was observed through, which
    -- with host is the scope one refresh replaces. A PAT row has none, and its
    -- host alone is its scope.
    installation_id  TEXT,
    host             TEXT NOT NULL CHECK (host <> ''),
    source           TEXT NOT NULL DEFAULT 'github',
    owner            TEXT NOT NULL,
    repo             TEXT NOT NULL,
    external_id      TEXT,
    description      TEXT NOT NULL DEFAULT '',
    language         TEXT NOT NULL DEFAULT '',
    html_url         TEXT NOT NULL DEFAULT '',
    pushed_at        TEXT NOT NULL DEFAULT '',
    private          INTEGER NOT NULL DEFAULT 0,
    observed_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((credential_class IN ('byo_app', 'managed_app') AND installation_id IS NOT NULL)
        OR (credential_class = 'pat' AND installation_id IS NULL)),
    FOREIGN KEY (org_id, host, installation_id)
        REFERENCES org_github_app_installations (org_id, github_host, installation_id)
        ON DELETE CASCADE
);

-- An App row takes its installation's host. The old key held one installation
-- per id in an org, so the join finds exactly one.
INSERT INTO reachable_repositories_new
    (org_id, credential_class, installation_id, host, source, owner, repo, external_id,
     description, language, html_url, pushed_at, private, observed_at)
SELECT r.org_id, r.credential_class, r.installation_id, i.github_host,
       r.source, r.owner, r.repo, r.external_id,
       r.description, r.language, r.html_url, r.pushed_at, r.private, r.observed_at
  FROM reachable_repositories r
  JOIN org_github_app_installations i
    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
 WHERE r.credential_class <> 'pat';

INSERT INTO reachable_repositories_new
    (org_id, credential_class, installation_id, host, source, owner, repo, external_id,
     description, language, html_url, pushed_at, private, observed_at)
SELECT r.org_id, r.credential_class, NULL, COALESCE(rc.canonical, r.host),
       r.source, r.owner, r.repo, r.external_id,
       r.description, r.language, r.html_url, r.pushed_at, r.private, r.observed_at
  FROM reachable_repositories r
  LEFT JOIN host_case rc ON rc.raw = r.host
 WHERE r.credential_class = 'pat'
   AND NOT EXISTS (
       SELECT 1
         FROM reachable_repositories o
         JOIN host_case oc ON oc.raw = o.host
        WHERE o.credential_class = 'pat'
          AND o.org_id = r.org_id
          AND lower(o.owner) = lower(r.owner)
          AND lower(o.repo) = lower(r.repo)
          AND o.host <> r.host
          AND oc.canonical = rc.canonical
          AND (o.observed_at, oc.raw = oc.canonical, o.host)
            > (r.observed_at, rc.raw = rc.canonical, r.host));

CREATE TABLE reachable_scopes_new (
    org_id           TEXT NOT NULL,
    credential_class TEXT NOT NULL
        CHECK (credential_class IN ('pat', 'byo_app', 'managed_app')),
    -- host: the GitHub the scope is on. scope: what one refresh replaces on
    -- it, the installation id for the App classes and the host again for pat.
    host             TEXT NOT NULL CHECK (host <> ''),
    scope            TEXT NOT NULL,
    refreshed_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, credential_class, host, scope)
);

INSERT INTO reachable_scopes_new (org_id, credential_class, host, scope, refreshed_at)
SELECT sc.org_id, sc.credential_class, i.github_host, sc.scope, sc.refreshed_at
  FROM reachable_scopes sc
  JOIN org_github_app_installations i
    ON i.org_id = sc.org_id AND i.installation_id = sc.scope
 WHERE sc.credential_class <> 'pat';

INSERT INTO reachable_scopes_new (org_id, credential_class, host, scope, refreshed_at)
SELECT sc.org_id, sc.credential_class,
       COALESCE(rc.canonical, sc.scope), COALESCE(rc.canonical, sc.scope), sc.refreshed_at
  FROM reachable_scopes sc
  LEFT JOIN host_case rc ON rc.raw = sc.scope
 WHERE sc.credential_class = 'pat'
   AND NOT EXISTS (
       SELECT 1
         FROM reachable_scopes o
         JOIN host_case oc ON oc.raw = o.scope
        WHERE o.credential_class = 'pat'
          AND o.org_id = sc.org_id
          AND o.scope <> sc.scope
          AND oc.canonical = rc.canonical
          AND (o.refreshed_at, oc.raw = oc.canonical, o.scope)
            > (sc.refreshed_at, rc.raw = rc.canonical, sc.scope));

DROP TABLE reachable_scopes;
DROP TABLE reachable_repositories;
DROP TABLE org_github_app_installations;

ALTER TABLE org_github_app_installations_new RENAME TO org_github_app_installations;
ALTER TABLE reachable_repositories_new RENAME TO reachable_repositories;
ALTER TABLE reachable_scopes_new RENAME TO reachable_scopes;

CREATE UNIQUE INDEX org_github_app_installations_active_account_key
    ON org_github_app_installations (org_id, github_host, account_login)
    WHERE removed_at IS NULL;
CREATE UNIQUE INDEX org_github_app_installations_active_host_installation_key
    ON org_github_app_installations (github_host, installation_id)
    WHERE removed_at IS NULL;
CREATE INDEX org_github_app_installations_org_idx
    ON org_github_app_installations (org_id);

CREATE UNIQUE INDEX reachable_repositories_app_identity
    ON reachable_repositories (org_id, host, installation_id, LOWER(owner), LOWER(repo))
    WHERE credential_class IN ('byo_app', 'managed_app');
CREATE UNIQUE INDEX reachable_repositories_pat_identity
    ON reachable_repositories (org_id, host, LOWER(owner), LOWER(repo))
    WHERE credential_class = 'pat';
CREATE INDEX reachable_repositories_registry_join
    ON reachable_repositories (org_id, source, LOWER(owner), LOWER(repo));
CREATE INDEX reachable_repositories_picker
    ON reachable_repositories (org_id, credential_class, LOWER(owner), LOWER(repo));

-- ---------------------------------------------------------------------------
-- 3f. artifacts and external_actions: scope and node_id
-- ---------------------------------------------------------------------------
--
-- A comment row exists only for a comment TF posted, so a reader tells TF's
-- comments from people's by whether a row exists. An edit or a delete of a
-- comment TF had not posted also left a row. The posting verb always stamps
-- the row's target (owner/repo#N), the edit and delete verbs never did, and
-- the store never lets an empty target overwrite a stamped one, so a GitHub
-- comment row with no target is one of those. The audit ledger keeps a row
-- for each such edit and delete.
DELETE FROM artifacts
 WHERE provider = 'github' AND kind = 'comment' AND target = '';

-- scope: the provider namespace the row is about (domain.ExternalObjectScope),
-- which joins the dedup key, (org_id, scope, dedup_key). The same owner/repo#18
-- exists on two GitHub hosts. Adding a column to a unique key only relaxes it,
-- so neither rebuilt index can fail on rows the old one held apart.
--
-- SQLite adds a NOT NULL column only with a default. The stores never write an
-- empty scope (they refuse one), so the default only reaches the rows of a
-- provider this install has no scope for.
--
-- node_id: the backing object's GraphQL node id. A GitHub comment artifact
-- carries it, because a GraphQL delete names a comment by node id alone and
-- has to find the comment's row without its number
-- (ArtifactStore.GetByNodeIDSystem). The create verb stamps it on its
-- pr_created audit row, and the reconciler's PR backstop matches that row to
-- the pull request's artifact by it, because a node id survives a repository
-- rename and the row's target does not. On the ledger it is part of the
-- record of the act: written once, never updated. NULL when no writer learned
-- one.
ALTER TABLE artifacts ADD COLUMN scope TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN node_id TEXT;
ALTER TABLE external_actions ADD COLUMN scope TEXT NOT NULL DEFAULT '';
ALTER TABLE external_actions ADD COLUMN node_id TEXT;

UPDATE artifacts
   SET scope = (SELECT s.scope FROM org_scope s
                 WHERE s.org_id = artifacts.org_id AND s.provider = artifacts.provider)
 WHERE provider IN (SELECT provider FROM org_scope);

UPDATE external_actions
   SET scope = (SELECT s.scope FROM org_scope s
                 WHERE s.org_id = external_actions.org_id AND s.provider = external_actions.provider)
 WHERE provider IN (SELECT provider FROM org_scope);

DROP INDEX idx_artifacts_dedup;
CREATE UNIQUE INDEX idx_artifacts_dedup ON artifacts (org_id, scope, dedup_key);

DROP INDEX idx_external_actions_dedup;
CREATE UNIQUE INDEX idx_external_actions_dedup ON external_actions (org_id, scope, dedup_key);

CREATE INDEX idx_artifacts_node_id ON artifacts (org_id, scope, node_id) WHERE node_id IS NOT NULL;

-- The GitHub poll tells a review TF submitted from one a person submitted by
-- whether a review artifact carries the review's numeric id as its external_id
-- (ArtifactStore.PresentExternalIDsSystem). It reads once per pull request
-- with new reviews; without this index each read walks every artifact the org
-- holds on the host.
CREATE INDEX idx_artifacts_external_id ON artifacts (org_id, scope, kind, external_id) WHERE external_id IS NOT NULL;

-- The PR backstop's read (ExternalActionStore.ListPRCreatedWithoutArtifactSystem)
-- runs every reconcile cycle and wants one org's pr_created rows from a recent
-- window. idx_external_actions_org_occurred narrows on org and time but reads
-- every action in the window; this one reads only the pr_created rows.
CREATE INDEX idx_external_actions_org_action_occurred ON external_actions (org_id, action, occurred_at);

-- ---------------------------------------------------------------------------
-- 3g. placement_overrides.host
-- ---------------------------------------------------------------------------
--
-- A repo pin is keyed on its owner/repo, which is unique only within one GitHub
-- host; repo is the only key kind TF writes, so every pin takes the org's
-- GitHub host. Rebuilt because host joins the primary key.
CREATE TABLE placement_overrides_new (
    org_id             TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    key_kind           TEXT NOT NULL,
    host               TEXT NOT NULL,
    key_value          TEXT NOT NULL,
    pinned_instance_id TEXT,
    replicas           INTEGER NOT NULL DEFAULT 0,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, key_kind, host, key_value)
);

INSERT INTO placement_overrides_new
    (org_id, key_kind, host, key_value, pinned_instance_id, replicas, updated_at)
SELECT p.org_id, p.key_kind, s.scope, p.key_value, p.pinned_instance_id, p.replicas, p.updated_at
  FROM placement_overrides p
  JOIN org_scope s ON s.org_id = p.org_id AND s.provider = 'github';

DROP TABLE placement_overrides;
ALTER TABLE placement_overrides_new RENAME TO placement_overrides;

-- ===========================================================================
-- 4. The Jira project registry
-- ===========================================================================
--
-- A team's Jira tracked set references a registry row instead of a project
-- key. A project key is a display value: Jira lets an admin rename it, and the
-- project keeps its id. While the tracked set held keys, a rename left every
-- rule naming a key no issue carried any more. jira_project_status_rules and
-- team_settings.jira_projects hold the registry row's id, and a rename changes
-- only the row's project_key.
--
-- A registry row is one project on one Jira site. external_id is Jira's
-- numeric project id, unique per (org, site); a row this migration could not
-- find one for keeps it NULL until the poll, or the tracker reading one of its
-- issues, resolves it by its key.

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

-- jira_entity is every Jira entity with the site 3c keyed it under, its key's
-- project key (NULL for a key with no '-', which names no project), and the
-- project id its snapshot records, if any. A snapshot records the project the
-- issue was in when it was read. json_extract raises on malformed JSON, and a
-- WHERE clause does not promise to test json_valid first, so the CASE guards
-- it.
CREATE TEMP TABLE jira_entity AS
SELECT e.id AS entity_id, e.org_id, es.scope AS site, e.source_id AS issue_key,
       CASE WHEN instr(e.source_id, '-') > 1
            THEN substr(e.source_id, 1, instr(e.source_id, '-') - 1) END AS project_key,
       NULLIF(CAST(CASE WHEN json_valid(e.snapshot_json)
                        THEN json_extract(e.snapshot_json, '$.project_id') END AS TEXT), '') AS external_id,
       e.last_polled_at AS polled_at
  FROM entities e
  JOIN entity_scope es ON es.entity_id = e.id
 WHERE e.source = 'jira';

-- jira_site is the site each org's rules are registered under: the org's Jira
-- scope from org_scope, which is domain.EntityScope("jira", settings). An org
-- with no usable base URL takes the site its Jira issues were last polled
-- from, which is where its rules last applied. An org with neither never
-- polled Jira with these rules, and its rules are not carried: a registry row
-- cannot be keyed without a site.
CREATE TEMP TABLE jira_site AS
SELECT org_id, site
  FROM (SELECT s.org_id, COALESCE(NULLIF(s.scope, ''),
               (SELECT j.site FROM jira_entity j
                 WHERE j.org_id = s.org_id AND j.site <> ''
                 ORDER BY j.polled_at DESC, j.entity_id
                 LIMIT 1)) AS site
          FROM org_scope s
         WHERE s.provider = 'jira')
 WHERE site IS NOT NULL;

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

CREATE TEMP TABLE jira_evidence AS
SELECT org_id, site, project_key, external_id, polled_at, entity_id
  FROM jira_entity
 WHERE project_key IS NOT NULL AND external_id IS NOT NULL;

UPDATE jira_seed
   SET (external_id, polled_at) = (
       SELECT v.external_id, v.polled_at
         FROM jira_evidence v
        WHERE v.org_id = jira_seed.org_id AND v.site = jira_seed.site
          AND v.project_key = jira_seed.project_key
        ORDER BY v.polled_at DESC, v.entity_id
        LIMIT 1);

-- Two keys can carry the same project id: a team that tracked a project under
-- its old key and re-added it under the new one after a rename. The id goes to
-- the key with the most recent evidence, which is the project's key now; the
-- others stay unresolved, and the poll merges each into this one when it
-- resolves its key. Evidence with no poll time sorts below any that has one,
-- and the key settles a tie, so exactly one key per id keeps it.
UPDATE jira_seed
   SET external_id = NULL
 WHERE rowid IN (
       SELECT rid FROM (
           SELECT rowid AS rid,
                  row_number() OVER (PARTITION BY org_id, site, external_id
                                     ORDER BY polled_at DESC, project_key) AS n
             FROM jira_seed
            WHERE external_id IS NOT NULL)
        WHERE n > 1);

-- randomblob()/random() are evaluated once per row, so each row gets its own
-- id; the shape is a v4 uuid, as everywhere else in this schema.
INSERT INTO jira_projects (id, org_id, site, external_id, project_key)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       org_id, site, external_id, project_key
  FROM jira_seed;

-- Every Jira issue entity references the registry row of its project as well
-- (section 5), so the registry also gets a row for every project TF holds an
-- issue in, whether or not a team tracks it.
--
-- An issue whose snapshot names its project id references the row carrying
-- that id on its site. When none does, one is created under the project key
-- of the most recently polled issue there that names the id, which is the
-- project's key as last read: the identity index keeps the first insert for an
-- id, and rows the rules already registered stand.
INSERT OR IGNORE INTO jira_projects (id, org_id, site, external_id, project_key)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       org_id, site, external_id, COALESCE(project_key, issue_key)
  FROM jira_entity
 WHERE site <> '' AND external_id IS NOT NULL
 ORDER BY polled_at DESC, entity_id;

-- An issue with no project id in its snapshot (a stub nothing polled yet, or a
-- snapshot from before project ids were recorded) references the row on its
-- site holding its key's project key. When no row does, an unresolved one is
-- created, which the poll resolves if a team tracks the project and the
-- tracker resolves when it next reads the issue.
INSERT OR IGNORE INTO jira_projects (id, org_id, site, project_key)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       j.org_id, j.site, COALESCE(j.project_key, j.issue_key)
  FROM jira_entity j
 WHERE j.site <> '' AND j.external_id IS NULL
   AND NOT EXISTS (
       SELECT 1 FROM jira_projects p
        WHERE p.org_id = j.org_id AND p.site = j.site
          AND p.project_key = COALESCE(j.project_key, j.issue_key));

-- jira_entity_project is the row each issue references: by its project id when
-- its snapshot names one, else by its key's project key, a resolved row first.
-- An issue with no site gets none (section 5).
CREATE TEMP TABLE jira_entity_project (
    entity_id       TEXT PRIMARY KEY,
    jira_project_id TEXT NOT NULL
);

INSERT INTO jira_entity_project (entity_id, jira_project_id)
SELECT j.entity_id,
       COALESCE(
           (SELECT p.id FROM jira_projects p
             WHERE p.org_id = j.org_id AND p.site = j.site AND p.external_id = j.external_id),
           (SELECT p.id FROM jira_projects p
             WHERE p.org_id = j.org_id AND p.site = j.site
               AND p.project_key = COALESCE(j.project_key, j.issue_key)
             ORDER BY p.external_id IS NULL, p.id
             LIMIT 1))
  FROM jira_entity j
 WHERE j.site <> '';

-- ---------------------------------------------------------------------------
-- 4a. jira_project_status_rules: (team_id, project_key) -> (team_id, jira_project_id)
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
-- 4b. team_settings.jira_projects: keys -> registry row ids, order kept
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

DROP TABLE jira_entity;
DROP TABLE jira_evidence;
DROP TABLE jira_seed;
DROP TABLE jira_site;

-- ===========================================================================
-- 5. entities
-- ===========================================================================
--
-- The rebuild 3c prepared: each row takes the scope entity_scope gave it, a
-- GitHub row the repository github_entity_repo resolved and the node id
-- github_entity_node gave it, and a Jira row the issue id jira_entity_issue
-- gave it and the project registry row jira_entity_project resolved in
-- section 4.
--
-- The CHECK lets a Jira row with no scope go without a project: a row whose
-- url names no site, in an org with no Jira site configured, has no site a
-- registry row could be keyed under. No read asks for an empty scope and no
-- write creates one (the store refuses it), so such a row is inert.

CREATE TABLE entities_new (
    id                       TEXT PRIMARY KEY,
    source                   TEXT NOT NULL,
    scope                    TEXT NOT NULL,
    source_id                TEXT NOT NULL,
    external_id              TEXT,
    kind                     TEXT NOT NULL,
    title                    TEXT,
    url                      TEXT,
    snapshot_json            TEXT,
    description              TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL DEFAULT 'active',
    created_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_polled_at           DATETIME,
    closed_at                DATETIME,
    owning_team_id           TEXT REFERENCES teams(id) ON DELETE SET NULL,
    org_id                   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    poll_seq                 INTEGER NOT NULL DEFAULT 0,
    commissioned_by_user_id  TEXT REFERENCES users(id) ON DELETE SET NULL,
    linear_team_id           TEXT,
    repository_id            TEXT REFERENCES repositories(id),
    jira_project_id          TEXT REFERENCES jira_projects(id),
    CONSTRAINT entities_repository_required CHECK (source <> 'github' OR repository_id IS NOT NULL),
    CONSTRAINT entities_jira_project_required CHECK (source <> 'jira' OR jira_project_id IS NOT NULL OR scope = '')
);

INSERT INTO entities_new (
    id, source, scope, source_id, external_id, kind, title, url, snapshot_json,
    description, state, created_at, last_polled_at, closed_at, owning_team_id,
    org_id, poll_seq, commissioned_by_user_id, repository_id, jira_project_id
)
SELECT
    e.id, e.source, es.scope,
    e.source_id,
    COALESCE(
        (SELECT CAST(n.node_id AS TEXT) FROM github_entity_node n WHERE n.entity_id = e.id),
        (SELECT CAST(j.issue_id AS TEXT) FROM jira_entity_issue j WHERE j.entity_id = e.id)),
    e.kind, e.title, e.url, e.snapshot_json,
    e.description, e.state, e.created_at, e.last_polled_at, e.closed_at, e.owning_team_id,
    e.org_id, e.poll_seq, e.commissioned_by_user_id,
    (SELECT r.id
       FROM github_entity_repo g
       JOIN repositories r
         ON r.org_id = g.org_id AND r.source = 'github' AND r.host = g.host
        AND LOWER(r.owner) = LOWER(g.owner) AND LOWER(r.repo) = LOWER(g.repo)
      WHERE g.entity_id = e.id),
    (SELECT j.jira_project_id FROM jira_entity_project j WHERE j.entity_id = e.id)
FROM entities e
JOIN entity_scope es ON es.entity_id = e.id;

DROP TABLE entities;
ALTER TABLE entities_new RENAME TO entities;

CREATE INDEX        idx_entities_state           ON entities(state);
CREATE INDEX        idx_entities_source_polled   ON entities(source, last_polled_at);
CREATE INDEX        idx_entities_closed_at       ON entities(closed_at) WHERE closed_at IS NOT NULL;
CREATE UNIQUE INDEX entities_id_org_unique       ON entities (id, org_id);
CREATE INDEX        idx_entities_github_author
    ON entities (json_extract(snapshot_json, '$.author'))
    WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
      AND json_valid(snapshot_json);
-- The author's GitHub account id, which a rename leaves alone: the dashboard
-- and team pull-request lists match an author by it when the snapshot and the
-- caller's binding both record one.
CREATE INDEX        idx_entities_github_author_id
    ON entities (json_extract(snapshot_json, '$.author_id'))
    WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
      AND json_valid(snapshot_json);
CREATE INDEX        idx_entities_commissioned_by
    ON entities (commissioned_by_user_id)
    WHERE commissioned_by_user_id IS NOT NULL;

-- An entity's identity: one row per provider id in a scope.
CREATE UNIQUE INDEX entities_identity
    ON entities (org_id, source, scope, external_id)
    WHERE external_id IS NOT NULL;

-- A key names one live object in a scope; closed rows may share it.
CREATE UNIQUE INDEX entities_active_key
    ON entities (org_id, source, scope, source_id)
    WHERE state = 'active';

-- The key lookup, which reads the active row or else the most recently closed.
CREATE INDEX        idx_entities_key
    ON entities (org_id, source, scope, source_id);

-- Serves the joins that scope a GitHub entity by its repository.
CREATE INDEX        idx_entities_repository
    ON entities (repository_id)
    WHERE repository_id IS NOT NULL;

-- Serves the registry merge's move of a project's issues.
CREATE INDEX        idx_entities_jira_project
    ON entities (jira_project_id)
    WHERE jira_project_id IS NOT NULL;

-- A Jira or Linear row carries the provider's id for its issue, which is what
-- follows the issue across a key change (GetByExternalIDSystem, then
-- RenameSystem): every response that finds an issue names it, so no row is
-- created without one and none loses the one it has. Triggers rather than a
-- CHECK because they constrain writes and not the rows already here: a Jira
-- row jira_entity_issue found no id for stays, and learns it from Jira (3c).
-- Linear has no such rows, since nothing before this migration polled it.
-- +goose StatementBegin
CREATE TRIGGER entities_external_id_required
BEFORE INSERT ON entities
WHEN NEW.source IN ('jira', 'linear') AND COALESCE(NEW.external_id, '') = ''
BEGIN
    SELECT RAISE(ABORT, 'entities_external_id_required: a jira or linear entity is created with its issue id');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER entities_external_id_kept
BEFORE UPDATE OF external_id ON entities
WHEN NEW.source IN ('jira', 'linear') AND COALESCE(NEW.external_id, '') = ''
     AND COALESCE(OLD.external_id, '') <> ''
BEGIN
    SELECT RAISE(ABORT, 'entities_external_id_required: a jira or linear entity keeps its issue id');
END;
-- +goose StatementEnd

DROP TABLE jira_entity_project;
DROP TABLE jira_entity_issue;
DROP TABLE github_entity_repo;
DROP TABLE github_entity_node;
DROP TABLE entity_scope;

-- ===========================================================================
-- 6. Pull request, review and branch artifacts keyed on ids
-- ===========================================================================
--
-- These artifacts are keyed on an id rather than their repository's name
-- (domain.ArtifactDedupKey):
--
--   pull_request  github:pull_request:<the pull request's node id>
--   review        github:review:<the pull request's node id>:<conversation id>
--   branch        git:branch:<the repository's registry row id>:<ref>
--
-- A name is given to other repositories: one created under a deleted
-- repository's name, or renamed onto it, would otherwise land its pull request
-- #N and its branches on the old repository's rows. target keeps the name, as
-- a display value.
--
-- Each existing row is re-keyed from what it records:
--
--   pull_request  the node id on the row, or else the one in its details,
--                 where every create recorded it, or else the PR entity's its
--                 target names in its scope (section 5 gave a polled pull
--                 request its node id). The row's node_id takes it too, as
--                 every create writes it now.
--   review        the node id of the pull request its target names in its
--                 scope: the oldest pull_request row's that names it (the one
--                 re-keyed above), else the PR entity's (section 5 gave a
--                 polled pull request its node id). The conversation id in
--                 the key stays.
--   branch        the live registry row its target names on its scope. The
--                 ref in the key stays.
--
-- Names compare case-insensitively, as GitHub's do. A row that resolves to
-- nothing keeps its key. Nothing writes that shape, so it is history no
-- writer lands on again.
--
-- One object recorded under two spellings of its repository's name — before a
-- rename was applied, or in another case — re-keys to one key. The older row
-- (created_at, then id) takes it and the newer is deleted: both describe the
-- one object, and the older is the one its first writer recorded. No table
-- references an artifact row.
CREATE TEMP TABLE pr_artifact_node AS
SELECT id, org_id, scope, target, created_at, node_id
  FROM (SELECT a.id, a.org_id, a.scope, a.target, a.created_at,
               COALESCE(NULLIF(a.node_id, ''),
                        CASE WHEN json_valid(a.details_json)
                             THEN NULLIF(CAST(json_extract(a.details_json, '$.node_id') AS TEXT), '') END,
                        (SELECT e.external_id FROM entities e
                          WHERE e.org_id = a.org_id AND e.source = 'github' AND e.scope = a.scope
                            AND LOWER(e.source_id) = LOWER(a.target) AND COALESCE(e.external_id, '') <> ''
                          ORDER BY e.created_at, e.id LIMIT 1)) AS node_id
          FROM artifacts a
         WHERE a.provider = 'github' AND a.kind = 'pull_request')
 WHERE node_id IS NOT NULL;

CREATE TEMP TABLE artifact_rekey (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    scope      TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    new_key    TEXT NOT NULL
);

INSERT INTO artifact_rekey (id, org_id, scope, created_at, new_key)
SELECT id, org_id, scope, created_at, 'github:pull_request:' || node_id
  FROM pr_artifact_node;

-- rest is the key after 'github:review:' (14 characters): the target, then
-- ':' and the conversation id when the key has one. Neither holds a ':'.
INSERT INTO artifact_rekey (id, org_id, scope, created_at, new_key)
SELECT id, org_id, scope, created_at,
       'github:review:' || node_id ||
       CASE WHEN instr(rest, ':') > 0 THEN substr(rest, instr(rest, ':')) ELSE '' END
  FROM (SELECT r.id, r.org_id, r.scope, r.created_at,
               substr(r.dedup_key, 15) AS rest,
               COALESCE(
                   (SELECT p.node_id FROM pr_artifact_node p
                     WHERE p.org_id = r.org_id AND p.scope = r.scope AND LOWER(p.target) = LOWER(r.target)
                     ORDER BY p.created_at, p.id LIMIT 1),
                   (SELECT e.external_id FROM entities e
                     WHERE e.org_id = r.org_id AND e.source = 'github' AND e.scope = r.scope
                       AND LOWER(e.source_id) = LOWER(r.target) AND COALESCE(e.external_id, '') <> ''
                     ORDER BY e.created_at, e.id LIMIT 1)) AS node_id
          FROM artifacts r
         WHERE r.provider = 'github' AND r.kind = 'review'
           AND substr(r.dedup_key, 1, 14) = 'github:review:')
 WHERE node_id IS NOT NULL;

-- rest is the key after 'git:branch:' (11 characters): owner/repo, ':', the
-- ref.
INSERT INTO artifact_rekey (id, org_id, scope, created_at, new_key)
SELECT b.id, b.org_id, b.scope, b.created_at,
       'git:branch:' || repo.id || substr(b.rest, instr(b.rest, ':'))
  FROM (SELECT a.id, a.org_id, a.scope, a.target, a.created_at, substr(a.dedup_key, 12) AS rest
          FROM artifacts a
         WHERE a.provider = 'git' AND a.kind = 'branch'
           AND substr(a.dedup_key, 1, 11) = 'git:branch:') b
  JOIN repositories repo
    ON repo.org_id = b.org_id AND repo.source = 'github' AND repo.host = b.scope
   AND repo.retired_at IS NULL
   AND LOWER(repo.owner || '/' || repo.repo) = LOWER(b.target)
 WHERE instr(b.rest, ':') > 0 AND instr(b.rest, ':') < length(b.rest);

DELETE FROM artifacts
 WHERE id IN (SELECT id
                FROM (SELECT id, row_number() OVER (PARTITION BY org_id, scope, new_key
                                                    ORDER BY created_at, id) AS n
                        FROM artifact_rekey)
               WHERE n > 1);

UPDATE artifacts
   SET dedup_key = (SELECT k.new_key FROM artifact_rekey k WHERE k.id = artifacts.id)
 WHERE id IN (SELECT id FROM artifact_rekey);

UPDATE artifacts
   SET node_id = (SELECT p.node_id FROM pr_artifact_node p WHERE p.id = artifacts.id)
 WHERE node_id IS NULL AND id IN (SELECT id FROM pr_artifact_node);

DROP TABLE artifact_rekey;
DROP TABLE pr_artifact_node;

-- ===========================================================================
-- 7. Artifacts reference the entity they are about
-- ===========================================================================
--
-- artifacts.entity_id is the entity an artifact is about
-- (domain.Artifact.EntityID): the pull request a pull request, review or
-- comment row is on, the Jira or Linear issue. A branch has none. target is
-- that entity's key, and every write that moves a key (an entity's rename, a
-- repository's, the merge or fold of two entities) moves the targets of the
-- entity's artifacts by it. SET NULL keeps an artifact, unlinked, if its
-- entity is deleted; a merge re-points it first.
ALTER TABLE artifacts ADD COLUMN entity_id TEXT REFERENCES entities(id) ON DELETE SET NULL;
CREATE INDEX idx_artifacts_entity ON artifacts (entity_id) WHERE entity_id IS NOT NULL;

-- Each existing row is linked to the entity carrying the provider id its key
-- names in its scope (a pull request's or review's node id, a Jira issue's id,
-- a Linear issue's UUID), or else to the entity its target names there,
-- compared case-insensitively: an active one first, then the newest. resource
-- is the key's segment after provider:kind:, up to the next ':'.
CREATE TEMP TABLE artifact_subject AS
SELECT id, org_id, provider, kind, scope, target,
       CASE WHEN instr(rest, ':') > 0 THEN substr(rest, 1, instr(rest, ':') - 1) ELSE rest END AS resource
  FROM (SELECT id, org_id, provider, kind, scope, target,
               substr(dedup_key, length(provider) + length(kind) + 3) AS rest
          FROM artifacts
         WHERE provider IN ('github', 'jira', 'linear', 'slack')
           AND substr(dedup_key, 1, length(provider) + length(kind) + 2) = provider || ':' || kind || ':');

UPDATE artifacts
   SET entity_id = (
       SELECT COALESCE(
                  (SELECT e.id FROM entities e
                    WHERE e.org_id = s.org_id AND e.source = s.provider AND e.scope = s.scope
                      AND e.external_id = CASE
                          WHEN s.provider = 'github' AND s.kind IN ('pull_request', 'review')
                               AND instr(s.resource, '#') = 0 THEN s.resource
                          WHEN s.provider = 'jira' AND instr(s.resource, '/') > 0
                               THEN substr(s.resource, instr(s.resource, '/') + 1)
                          WHEN s.provider = 'linear' THEN s.resource
                          END
                    ORDER BY e.created_at, e.id LIMIT 1),
                  (SELECT e.id FROM entities e
                    WHERE e.org_id = s.org_id AND e.source = s.provider AND e.scope = s.scope
                      AND LOWER(e.source_id) = LOWER(s.target)
                    ORDER BY e.state = 'active' DESC, e.created_at DESC, e.id LIMIT 1))
         FROM artifact_subject s
        WHERE s.id = artifacts.id)
 WHERE id IN (SELECT id FROM artifact_subject);

UPDATE artifacts
   SET target = (SELECT e.source_id FROM entities e WHERE e.id = artifacts.entity_id)
 WHERE entity_id IS NOT NULL
   AND target <> (SELECT e.source_id FROM entities e WHERE e.id = artifacts.entity_id);

-- A Jira artifact is keyed on its issue's site and id (domain.JiraIssueResource:
-- the query-escaped site, '/', the id). Rows recorded before that carry the
-- issue key (jira:issue:<KEY>) or, for a comment, the comment id alone
-- (jira:comment:<comment id>), and are re-keyed from the issue their entity
-- names. The site is escaped by replacing '%', ':' and '/', which is
-- url.QueryEscape for a site holding no other character it escapes; a row on
-- a site that does, or with no entity carrying an issue id, keeps its key.
-- One issue recorded under both shapes keeps the older row, as section 6 does.
CREATE TEMP TABLE jira_rekey (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    scope      TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    new_key    TEXT NOT NULL
);

INSERT INTO jira_rekey (id, org_id, scope, created_at, new_key)
SELECT a.id, a.org_id, a.scope, a.created_at,
       'jira:' || a.kind || ':' ||
       replace(replace(replace(a.scope, '%', '%25'), ':', '%3A'), '/', '%2F') || '/' || e.external_id ||
       CASE WHEN a.kind = 'comment' THEN ':' || s.resource ELSE '' END
  FROM artifacts a
  JOIN artifact_subject s ON s.id = a.id
  JOIN entities e ON e.id = a.entity_id
 WHERE a.provider = 'jira' AND a.kind IN ('issue', 'comment')
   AND instr(s.resource, '/') = 0
   AND a.dedup_key = 'jira:' || a.kind || ':' || s.resource
   AND COALESCE(e.external_id, '') <> ''
   AND a.scope <> '' AND a.scope NOT GLOB '*[^-A-Za-z0-9._~:/%]*';

DELETE FROM artifacts
 WHERE id IN (SELECT id
                FROM (SELECT id, row_number() OVER (PARTITION BY org_id, scope, k
                                                    ORDER BY created_at, id) AS n
                        FROM (SELECT id, org_id, scope, created_at, new_key AS k FROM jira_rekey
                              UNION ALL
                              SELECT a.id, a.org_id, a.scope, a.created_at, a.dedup_key
                                FROM artifacts a
                               WHERE a.id NOT IN (SELECT id FROM jira_rekey)
                                 AND EXISTS (SELECT 1 FROM jira_rekey r
                                              WHERE r.org_id = a.org_id AND r.scope = a.scope
                                                AND r.new_key = a.dedup_key)))
               WHERE n > 1);

UPDATE artifacts
   SET dedup_key = (SELECT k.new_key FROM jira_rekey k WHERE k.id = artifacts.id)
 WHERE id IN (SELECT id FROM jira_rekey);

DROP TABLE jira_rekey;
DROP TABLE artifact_subject;

-- ===========================================================================
-- 8. GitHub event-handler predicates name people and teams by GitHub's id
-- ===========================================================================
--
-- A GitHub predicate's identity filters take GitHub's ids, which a rename
-- leaves alone: author_id_in, reviewer_id_in and commenter_id_in for accounts,
-- requested_user_id_in and requested_team_id_in for a requested reviewer. The
-- login-valued filters they replace are unknown fields now, and a predicate
-- carrying one would refuse to match, so every stored GitHub predicate is
-- rewritten to the id fields:
--
--   author_in, reviewer_in, commenter_in   become author_id_in, reviewer_id_in
--                                          and commenter_id_in, each login
--                                          replaced by its account's id;
--   author, reviewer, commenter            a single login, folded into the
--                                          list above as its one entry, or,
--                                          beside a list of its own, narrowing
--                                          the list to it;
--   requested_login                        requested_user_id_in, one entry;
--   requested_team                         requested_team_id_in, one entry.
--
-- A login's id is the github_user_id of the identity binding that holds the
-- login on the org's GitHub host, the most recently verified one when several
-- do. A login no binding with an id holds is kept as written, and a team's
-- org/slug is always kept as written (the mappings 3d copied record no ids
-- yet): neither is an id, so the filter matches nothing until the handler is
-- edited, rather than matching everyone. For the same reason a singular login
-- that narrows its list to nothing leaves the list ["0"], which no account has
-- (GitHub numbers them from 1), where the pair before matched nothing too.
-- mentioned_login and mentioned_team name the handle a mention spelled and are
-- unchanged.
CREATE TEMP TABLE gh_login_id (
    host           TEXT NOT NULL,
    login          TEXT NOT NULL,
    github_user_id TEXT NOT NULL,
    PRIMARY KEY (host, login)
);

INSERT INTO gh_login_id (host, login, github_user_id)
SELECT github_base_url, lower(login), github_user_id
  FROM (SELECT github_base_url, login, github_user_id,
               row_number() OVER (PARTITION BY github_base_url, lower(login)
                                  ORDER BY verified_at DESC, user_id) AS n
          FROM user_github_identities
         WHERE github_user_id IS NOT NULL)
 WHERE n = 1;

CREATE TEMP TABLE gh_predicate (
    id   TEXT PRIMARY KEY,
    host TEXT NOT NULL,
    p    TEXT NOT NULL
);

INSERT INTO gh_predicate (id, host, p)
SELECT h.id, s.scope, h.scope_predicate_json
  FROM event_handlers h
  JOIN org_scope s ON s.org_id = h.org_id AND s.provider = 'github'
 WHERE h.event_type LIKE 'github:%'
   AND json_valid(h.scope_predicate_json)
   AND json_type(h.scope_predicate_json) = 'object';

-- The lists, renamed, each entry resolved.
UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.author_id_in', json(
             (SELECT json_group_array(COALESCE(
                       (SELECT m.github_user_id FROM gh_login_id m
                         WHERE m.host = gh_predicate.host AND m.login = lower(e.value)),
                       e.value))
                FROM json_each(gh_predicate.p, '$.author_in') e))), '$.author_in')
 WHERE json_type(p, '$.author_in') = 'array';

UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.reviewer_id_in', json(
             (SELECT json_group_array(COALESCE(
                       (SELECT m.github_user_id FROM gh_login_id m
                         WHERE m.host = gh_predicate.host AND m.login = lower(e.value)),
                       e.value))
                FROM json_each(gh_predicate.p, '$.reviewer_in') e))), '$.reviewer_in')
 WHERE json_type(p, '$.reviewer_in') = 'array';

UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.commenter_id_in', json(
             (SELECT json_group_array(COALESCE(
                       (SELECT m.github_user_id FROM gh_login_id m
                         WHERE m.host = gh_predicate.host AND m.login = lower(e.value)),
                       e.value))
                FROM json_each(gh_predicate.p, '$.commenter_in') e))), '$.commenter_in')
 WHERE json_type(p, '$.commenter_in') = 'array';

-- The singular logins, resolved the same way, and folded into their lists. An
-- entry the singular is compared with is resolved already, so the two compare
-- as ids, or as logins when neither resolved.
UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.author_id_in', json(
             CASE WHEN json_array_length(p, '$.author_id_in') > 0
                  THEN COALESCE(NULLIF(
                         (SELECT json_group_array(e.value) FROM json_each(gh_predicate.p, '$.author_id_in') e
                           WHERE lower(e.value) = lower(COALESCE(
                                   (SELECT m.github_user_id FROM gh_login_id m
                                     WHERE m.host = gh_predicate.host
                                       AND m.login = lower(json_extract(gh_predicate.p, '$.author'))),
                                   json_extract(gh_predicate.p, '$.author')))),
                         '[]'), '["0"]')
                  ELSE json_array(COALESCE(
                         (SELECT m.github_user_id FROM gh_login_id m
                           WHERE m.host = gh_predicate.host
                             AND m.login = lower(json_extract(gh_predicate.p, '$.author'))),
                         json_extract(gh_predicate.p, '$.author')))
             END)), '$.author')
 WHERE json_type(p, '$.author') = 'text';

UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.reviewer_id_in', json(
             CASE WHEN json_array_length(p, '$.reviewer_id_in') > 0
                  THEN COALESCE(NULLIF(
                         (SELECT json_group_array(e.value) FROM json_each(gh_predicate.p, '$.reviewer_id_in') e
                           WHERE lower(e.value) = lower(COALESCE(
                                   (SELECT m.github_user_id FROM gh_login_id m
                                     WHERE m.host = gh_predicate.host
                                       AND m.login = lower(json_extract(gh_predicate.p, '$.reviewer'))),
                                   json_extract(gh_predicate.p, '$.reviewer')))),
                         '[]'), '["0"]')
                  ELSE json_array(COALESCE(
                         (SELECT m.github_user_id FROM gh_login_id m
                           WHERE m.host = gh_predicate.host
                             AND m.login = lower(json_extract(gh_predicate.p, '$.reviewer'))),
                         json_extract(gh_predicate.p, '$.reviewer')))
             END)), '$.reviewer')
 WHERE json_type(p, '$.reviewer') = 'text';

UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.commenter_id_in', json(
             CASE WHEN json_array_length(p, '$.commenter_id_in') > 0
                  THEN COALESCE(NULLIF(
                         (SELECT json_group_array(e.value) FROM json_each(gh_predicate.p, '$.commenter_id_in') e
                           WHERE lower(e.value) = lower(COALESCE(
                                   (SELECT m.github_user_id FROM gh_login_id m
                                     WHERE m.host = gh_predicate.host
                                       AND m.login = lower(json_extract(gh_predicate.p, '$.commenter'))),
                                   json_extract(gh_predicate.p, '$.commenter')))),
                         '[]'), '["0"]')
                  ELSE json_array(COALESCE(
                         (SELECT m.github_user_id FROM gh_login_id m
                           WHERE m.host = gh_predicate.host
                             AND m.login = lower(json_extract(gh_predicate.p, '$.commenter'))),
                         json_extract(gh_predicate.p, '$.commenter')))
             END)), '$.commenter')
 WHERE json_type(p, '$.commenter') = 'text';

UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.requested_user_id_in', json(json_array(COALESCE(
             (SELECT m.github_user_id FROM gh_login_id m
               WHERE m.host = gh_predicate.host
                 AND m.login = lower(json_extract(gh_predicate.p, '$.requested_login'))),
             json_extract(p, '$.requested_login'))))), '$.requested_login')
 WHERE json_type(p, '$.requested_login') = 'text';

UPDATE gh_predicate
   SET p = json_remove(json_set(p, '$.requested_team_id_in', json(json_array(json_extract(p, '$.requested_team')))),
                       '$.requested_team')
 WHERE json_type(p, '$.requested_team') = 'text';

-- A null filter is no filter, as an absent one is, so a null old field is
-- dropped.
UPDATE gh_predicate SET p = json_remove(p, '$.author_in') WHERE json_type(p, '$.author_in') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.reviewer_in') WHERE json_type(p, '$.reviewer_in') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.commenter_in') WHERE json_type(p, '$.commenter_in') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.author') WHERE json_type(p, '$.author') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.reviewer') WHERE json_type(p, '$.reviewer') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.commenter') WHERE json_type(p, '$.commenter') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.requested_login') WHERE json_type(p, '$.requested_login') = 'null';
UPDATE gh_predicate SET p = json_remove(p, '$.requested_team') WHERE json_type(p, '$.requested_team') = 'null';

UPDATE event_handlers
   SET scope_predicate_json = (SELECT g.p FROM gh_predicate g WHERE g.id = event_handlers.id)
 WHERE id IN (SELECT g.id FROM gh_predicate g
                JOIN event_handlers h ON h.id = g.id
               WHERE g.p <> h.scope_predicate_json);

DROP TABLE gh_predicate;
DROP TABLE gh_login_id;

DROP TABLE org_scope;
DROP TABLE host_case;

COMMIT;

PRAGMA foreign_keys = on;

-- +goose Down
SELECT 'down not supported';
