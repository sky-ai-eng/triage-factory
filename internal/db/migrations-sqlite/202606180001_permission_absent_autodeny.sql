-- +goose Up
-- Presence-gated fast auto-deny for unattended permission prompts (TFAC-392).
-- When a delegated run raises an off-allowlist tool prompt and no answer-capable,
-- focused tab is present in the run's org, the backend denies after a short grace
-- instead of waiting the full permTimeout(). Both knobs are per-team:
--
--   permission_absent_grace_ms         — how long an unattended prompt waits for a
--                                         reconnect / tab focus / navigation before
--                                         denying with "no operator available".
--                                         Default 15s. Clamped at spawn to
--                                         [1s, permTimeout()) so it can never invert
--                                         the "total wait < idleTimeout()" invariant.
--   permission_absent_autodeny_enabled — master toggle. When 0 (off) the prompt keeps
--                                         today's behavior exactly (full permTimeout()),
--                                         so this is a byte-for-byte no-op when disabled.
--                                         Defaults 1 (on).
ALTER TABLE team_settings ADD COLUMN permission_absent_grace_ms INTEGER NOT NULL DEFAULT 15000;
ALTER TABLE team_settings ADD COLUMN permission_absent_autodeny_enabled INTEGER NOT NULL DEFAULT 1;

-- +goose Down
SELECT 'down not supported';
