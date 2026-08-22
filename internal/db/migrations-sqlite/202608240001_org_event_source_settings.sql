-- +goose Up
-- Move the two UNIFORM per-source settings — base_url, poll_interval — off
-- org_settings and onto org_event_sources, which was created for exactly
-- this: "the per-(org, source) integration settings currently spread across
-- org_settings land here later." github_clone_protocol and
-- github_credential_class stay on org_settings — they are GitHub-shaped
-- concepts with no analogue at another source, so moving them would just
-- relocate the same one-column-per-source problem rather than solve it.
--
-- Both new columns are NULLABLE, and that is a real semantic: NULL means "no
-- per-source override recorded", not "use a default". A source's registration
-- decides whether base_url means anything at all (a self-host story — GitHub
-- Enterprise Server, Jira Data Center; Slack has none and never gets a value
-- here) and whether poll_interval does (a push source has no cadence). The
-- application resolves NULL to domain.DefaultOrgSettings()'s 5-minute cadence
-- for github/jira, exactly matching the fallback org_settings.github_poll_interval
-- / jira_poll_interval already had as NOT NULL DEFAULT columns — this backfill
-- makes that default concrete instead of leaving it implicit.
ALTER TABLE org_event_sources ADD COLUMN base_url TEXT;
ALTER TABLE org_event_sources ADD COLUMN poll_interval TEXT;

-- Backfill github + jira from the columns being dropped below, in two steps
-- per kind rather than one INSERT .. SELECT .. ON CONFLICT: SQLite's upsert
-- clause only parses after a VALUES-form insert, not a SELECT-form one.
--
-- Step 1 mints a row for any org that doesn't have one yet (INSERT OR IGNORE
-- — the common case, since the admin pause switch is opt-in). Step 2 is a
-- plain UPDATE that sets base_url/poll_interval for every github row from
-- org_settings, whether step 1 just created it or it already existed from an
-- earlier admin pause — and because that UPDATE names only these two
-- columns, disabled / disabled_at / disabled_by on a pre-existing row are
-- left exactly as the pause left them, the same disjoint-column split
-- SetDisabled and SetGitHubCredentialClass already have on their own tables.
INSERT OR IGNORE INTO org_event_sources (org_id, kind, base_url, poll_interval)
SELECT org_id, 'github', github_base_url, github_poll_interval FROM org_settings;

UPDATE org_event_sources
   SET base_url      = (SELECT github_base_url FROM org_settings WHERE org_settings.org_id = org_event_sources.org_id),
       poll_interval = (SELECT github_poll_interval FROM org_settings WHERE org_settings.org_id = org_event_sources.org_id)
 WHERE kind = 'github';

INSERT OR IGNORE INTO org_event_sources (org_id, kind, base_url, poll_interval)
SELECT org_id, 'jira', jira_base_url, jira_poll_interval FROM org_settings;

UPDATE org_event_sources
   SET base_url      = (SELECT jira_base_url FROM org_settings WHERE org_settings.org_id = org_event_sources.org_id),
       poll_interval = (SELECT jira_poll_interval FROM org_settings WHERE org_settings.org_id = org_event_sources.org_id)
 WHERE kind = 'jira';

ALTER TABLE org_settings DROP COLUMN github_base_url;
ALTER TABLE org_settings DROP COLUMN github_poll_interval;
ALTER TABLE org_settings DROP COLUMN jira_base_url;
ALTER TABLE org_settings DROP COLUMN jira_poll_interval;

-- +goose Down
SELECT 'down not supported';
