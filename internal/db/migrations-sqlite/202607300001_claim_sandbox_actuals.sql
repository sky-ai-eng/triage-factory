-- +goose Up
-- Per-engagement measured sandbox cost: what one claim's jail actually
-- consumed, read from its cgroup (memory.peak, cpu.stat's usage_usec) at
-- teardown, before the group is removed. Ground truth for margin analysis and
-- capacity planning, and unbackfillable — a run's cgroup is gone the instant
-- it ends — so the columns land ahead of any consumer.
--
-- Distinct from the pre-allocated envelope: mem_budget_mb stays an author-set
-- profile config item, and these are what the run then really used. Distinct
-- from instance_stats too, which samples the whole host and so attributes a
-- concurrent workload's memory to whatever run happened to be live.
--
-- NULL means "not measured", never "measured zero": local mode has no sandbox
-- at all, a pre-5.19 kernel has no memory.peak (cpu_usec lands, peak_mem_mb
-- stays NULL), and a crashed teardown records neither. The recorded CPU
-- deliberately includes the gVisor sentry's systrap overhead — it is TF's cost
-- view of the run, not a billed quantity.
ALTER TABLE claims ADD COLUMN peak_mem_mb INTEGER;
ALTER TABLE claims ADD COLUMN cpu_usec    INTEGER;

-- +goose Down
SELECT 'down not supported';
