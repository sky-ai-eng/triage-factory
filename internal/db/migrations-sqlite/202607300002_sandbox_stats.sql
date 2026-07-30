-- +goose Up
-- sandbox_stats: the per-sandbox resource series — one row per live jail per
-- sampler tick, appended by the executor's existing instance-stat sampler and
-- read by the per-run usage-over-time surface.
--
-- This is the SHAPE of a run's consumption while it runs, which the claim's
-- end-state actuals (peak_mem_mb / cpu_usec, read once at teardown) cannot
-- give. The two disagree slightly by design: a periodic sampler misses the
-- sub-minute spikes the kernel high-watermark catches. The series is shape,
-- the teardown snapshot is truth, and they are never reconciled numerically.
--
-- cpu_usec_cum is CUMULATIVE, not a rate: a consumer derives rate from the
-- difference between two samples, so a dropped tick self-heals into a
-- wider-but-correct interval instead of leaving a gap that reads as idle.
--
-- Both metric columns are nullable for the same reason instance_stats' are: a
-- partial sample is still a useful row, so the sampler writes whatever it
-- could read. A tick that read NOTHING for a jail (torn down between the
-- registry snapshot and the file read) writes no row at all.
--
-- No FK to claims, matching the instance_stats family convention: the write
-- is a best-effort batch on a telemetry path that must never be rejected by
-- referential integrity, and the table is bounded by its own age reaper —
-- same ~30d retention, same hourly cadence as instance_stats. One family,
-- one policy.
--
-- Nothing local ever lands here: local mode never sandboxes, so there is no
-- jail to sample. The table exists in this dialect because the store is one
-- dual-dialect contract with one conformance suite.
CREATE TABLE sandbox_stats (
    claim_id        TEXT NOT NULL,
    at              DATETIME NOT NULL,
    mem_current_mb  INTEGER,
    cpu_usec_cum    BIGINT,
    -- The PK is also the per-claim series index (the read is "one claim,
    -- ordered by at"), and it makes a retried tick an idempotent no-op via
    -- ON CONFLICT DO NOTHING.
    PRIMARY KEY (claim_id, at)
);

-- The reaper deletes by age alone, which the PK's leading claim_id can't
-- serve — so a dedicated `at` index earns its keep, same as instance_stats.
CREATE INDEX idx_sandbox_stats_at ON sandbox_stats(at);

-- +goose Down
SELECT 'down not supported';
