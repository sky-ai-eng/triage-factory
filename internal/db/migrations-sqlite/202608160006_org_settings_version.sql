-- +goose Up
-- An optimistic-concurrency token on the org settings row.
--
-- The org settings save is a read-modify-write over the whole row: a client
-- fetches every field, edits one, and writes them all back. Two admins editing
-- different sections of the same page therefore overwrite each other silently —
-- the second save carries the first's fields as the client last saw them, so
-- the first admin's change is undone with a 200 and nothing to notice. The
-- version turns that into a detectable event: the read hands the client the
-- token it read at, the write requires it, and a write against a stale token is
-- refused so the loser can refetch and re-apply.
--
-- The counter belongs to the settings WRITER, not to the row's every touch:
-- UpdateSettings bumps it, SetGitHubCredentialClass (a surgical single-column
-- write owned by the credential transitions) does not, so a credential
-- transition never invalidates an admin's in-flight settings edit.
--
-- Day one: local mode is N=1 — a single user on a single machine, with no
-- second admin to race — so existing installs see no behavioural change beyond
-- an extra integer on the wire. The DEFAULT 1 backfills every existing row in
-- place, and a fresh row starts at 1 as well, so an upgraded install and a
-- fresh install are indistinguishable from the first read onwards. No data is
-- rewritten and nothing is dropped.
ALTER TABLE org_settings ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

-- +goose Down
SELECT 'down not supported';
