-- +goose Up
-- dashboard_backfilled_at marks that the one-shot trailing-window dashboard
-- history backfill has run for this (user, host) GitHub identity (TFAC-396).
--
-- Multi-mode App / org-PAT orgs have no per-cycle "me" backfill — an App
-- installation token (and a multi-mode org PAT) has no viewer, so the org poll
-- cycle can't run author:@me / reviewed-by:@me search. Without it the personal
-- dashboard's merged/closed/reviews counts and 14-day sparkline are blank for
-- any history that predates tracking. The server instead seeds that history
-- once per bound identity, keyed by the user's explicit login: on identity
-- bind, and lazily on first dashboard load for identities bound before this
-- shipped. NULL = not yet backfilled (eligible); a timestamp = done, so a
-- re-poll or repeated dashboard opens never re-fire the per-installation search
-- burst (its cost would otherwise scale with viewers on one shared installation
-- rate budget). UpsertGitHubIdentity resets this to NULL when the bound login
-- changes, so a rename / re-bind backfills the new login's history.
--
-- Local mode (N=1) ignores this column: its per-cycle Phase 1b owns dashboard
-- history and is self-healing.
ALTER TABLE user_github_identities ADD COLUMN dashboard_backfilled_at TIMESTAMP;

-- +goose Down
SELECT 'down not supported';
