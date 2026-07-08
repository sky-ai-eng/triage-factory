-- +goose Up
-- instances: the fleet membership registry every TF process registers
-- into at boot and refreshes via periodic heartbeat. Role-neutral name on
-- purpose — every TF process registers (control pods too, for
-- deployment-wide visibility), not just executors; "executor" stays the
-- *role* name everywhere else (runs.executor_id keeps its shipped meaning:
-- the id of the instance acting as a run's executor).
--
-- Persistent identity lives OUTSIDE this table (a file under
-- <TF_STATE_ROOT>/instance-id, internal/instance) — id is minted once per
-- state root and re-read on every boot, so a restart keeps the same id and
-- Register below just bumps boot_epoch. SQLite is N=1: one process, one
-- row, epoch bumping per restart, trivially satisfying the same schema
-- Postgres uses for a real fleet.
--
-- max_runs / active_runs / mem_total_mb / mem_available_mb / dispatch_gated
-- move capacity + admission state that used to be process-local onto this
-- heartbeat row. NULL on pure-control rows (meaningless there) and, in
-- local mode, simply whatever the single process's own dispatcher
-- observes.
CREATE TABLE instances (
    id                 TEXT PRIMARY KEY,
    role               TEXT NOT NULL,
    version            TEXT NOT NULL,
    boot_epoch         INTEGER NOT NULL,
    started_at         DATETIME NOT NULL,
    last_heartbeat_at  DATETIME NOT NULL,
    draining           BOOLEAN NOT NULL DEFAULT 0,
    max_runs           INTEGER,
    active_runs        INTEGER,
    mem_total_mb       INTEGER,
    mem_available_mb   INTEGER,
    dispatch_gated     BOOLEAN,
    labels_json        TEXT
);

-- +goose Down
SELECT 'down not supported';
