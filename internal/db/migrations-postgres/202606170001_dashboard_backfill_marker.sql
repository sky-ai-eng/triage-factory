-- +goose Up
-- dashboard_backfilled_at marks that the one-shot trailing-window dashboard
-- history backfill has run for this (user, host) GitHub identity (TFAC-396).
-- See the SQLite sibling migration for the full rationale. NULL = eligible; a
-- timestamp = done. Written by the claims-free backfill worker through the
-- admin pool (MarkDashboardBackfilledSystem), so it carries no RLS policy of
-- its own beyond the table's existing per-user gates.
ALTER TABLE public.user_github_identities ADD COLUMN dashboard_backfilled_at timestamp with time zone;

-- +goose Down
SELECT 'down not supported';
