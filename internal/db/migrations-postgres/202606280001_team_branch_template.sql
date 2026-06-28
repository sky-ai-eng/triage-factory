-- +goose Up
-- Per-team branch-name template (TFAC-498): a free-text convention the
-- delegated agent is shown as envelope guidance (NOT enforced). The literal
-- "<ticket-id>" in the template is substituted with the run's ticket id at
-- prompt-render time. NOT NULL with a literal DEFAULT so existing rows and
-- partial upserts (e.g. SetDailyCostCapSystem) materialize the default without
-- the writer naming the column. The app layer coalesces an empty string to the
-- default on write so a blank never persists. Mirrors the SQLite twin
-- (202606280001) for schema parity.
ALTER TABLE public.team_settings ADD COLUMN branch_template text NOT NULL DEFAULT 'tfac/<ticket-id>';

-- +goose Down
SELECT 'down not supported';
