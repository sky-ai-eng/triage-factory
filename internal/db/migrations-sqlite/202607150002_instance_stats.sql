-- +goose Up
-- instance_stats: 1-minute telemetry samples, one row per pod per minute,
-- written by the sampler alongside the registry heartbeat and read by the
-- fleet dashboard's timeseries + overview surfaces. The registry row
-- (instances) carries the *current* capacity/admission snapshot; this table
-- carries its *history* plus the fields too transient for the 4s heartbeat
-- to be a useful record of (per-sample claim rate, spawn latency, OOM kills).
--
-- Every metric column is nullable on purpose: a control pod has no runs to
-- report (active_runs/queued_visible/claims/spawn_p50_ms stay NULL), and a
-- non-Linux or unconfined host can't report cpu/load/oom — a partial sample
-- is still a useful row, so the sampler writes whatever it could read rather
-- than dropping the sample. mem_available_mb comes from hostmem (cgroup-aware
-- instance truth), never a host-wide /proc/meminfo read.
--
-- System table, admin-pool-only in Postgres — mirrors instances (a fleet
-- member's telemetry isn't tenant data); SQLite is unscoped N=1. A ~30d
-- retention reaper trims it (the row volume is one insert/minute/pod).
CREATE TABLE instance_stats (
    instance_id       TEXT NOT NULL,
    at                DATETIME NOT NULL,
    active_runs       INTEGER,
    queued_visible    INTEGER,
    mem_available_mb  INTEGER,
    cpu_pct           REAL,
    load1             REAL,
    claims            INTEGER,
    spawn_p50_ms      INTEGER,
    oom_kills         INTEGER,
    PRIMARY KEY (instance_id, at)
);

-- The reaper deletes by age and the fleet-wide timeseries scans a recent
-- window across all instances, both keyed on `at` alone (the PK's leading
-- instance_id can't serve either) — so a dedicated `at` index earns its keep.
CREATE INDEX idx_instance_stats_at ON instance_stats(at);

-- +goose Down
SELECT 'down not supported';
