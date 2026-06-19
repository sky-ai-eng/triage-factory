-- +goose Up
-- Presence-gated fast auto-deny for unattended permission prompts (TFAC-392).
-- See the SQLite sibling migration for the full rationale. permission_absent_grace_ms
-- is the grace window (ms) an unattended prompt waits before denying with
-- "no operator available"; permission_absent_autodeny_enabled is the master toggle
-- (false = today's full-permTimeout() behavior, byte-for-byte). Both ship NOT NULL
-- with the same defaults as the SQLite tree and domain.DefaultTeamSettings().
ALTER TABLE public.team_settings ADD COLUMN permission_absent_grace_ms integer NOT NULL DEFAULT 15000;
ALTER TABLE public.team_settings ADD COLUMN permission_absent_autodeny_enabled boolean NOT NULL DEFAULT true;

-- +goose Down
SELECT 'down not supported';
