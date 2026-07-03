-- +goose Up
-- failure_kind is the machine-readable discriminator for WHY a run reached
-- status='failed' (domain.RunFailureKind: memory_limit / crash / no_result /
-- agent_error), written by MarkFailedIfActive and Complete alongside the
-- status flip. Same pattern as repo_profiles.clone_error_kind: the enum is
-- app-validated (no CHECK), free text stays in run_messages/result_summary,
-- and NULL means "no specific classification" — non-failed runs, rows failed
-- before this column existed, and failures nothing classified. The UI keys
-- kind-specific rendering (e.g. a memory-limit kill pointing at
-- TF_RUN_MEMORY_LIMIT_MB) off this value, never off message text.
ALTER TABLE runs ADD COLUMN failure_kind TEXT;

-- +goose Down
SELECT 'down not supported';
