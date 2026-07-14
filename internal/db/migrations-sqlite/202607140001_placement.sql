-- +goose Up
-- Placement affinity layer (TFAC-587, spec §6): capacity-weighted rendezvous
-- hashing keeps a repeatedly-hit repo's bare/worktree cache warm on one (or
-- K) executor instead of re-multiplying it across the fleet. Two additions,
-- both no-ops in local mode (N=1: the hash always returns self) — they land
-- here for store-interface + conformance-test symmetry across dialects, the
-- same rule instances / run_credentials followed.

-- runs.preferred_executor_id: the rendezvous winner for this run's
-- (org, repo) key, stamped by the control plane at enqueue and re-stamped on
-- every blueprint-step advance so it never outlives one queue dwell. The
-- two-tier claim reads it: tier 1 is an indexed equality (preferred = me),
-- tier 2 spills anything aged or whose preferred owner is dead/gated/
-- draining. Advisory only — a NULL (never stamped, or cleared on requeue)
-- means "unowned, claimable by anyone now", so dropping the whole layer can
-- never break a claim. Cleared wherever executor_id is (a queued row has no
-- owner and no preference); unlike executor_id it is also SET while the row
-- is queued (that IS its purpose).
ALTER TABLE runs ADD COLUMN preferred_executor_id TEXT;

-- placement_overrides: human intent that wins over the computed rendezvous
-- order for a single key — a manual pin, or a hot-key replica count, and
-- nothing else (drain lives on the executor's registry row). Checked before
-- the hash; expected to stay nearly empty (the hash needs no rows). Keyed by
-- (org_id, key_kind, key_value): key_kind is 'repo' (delegation,
-- "owner/repo") or 'project' (curator). System table, admin-pool-only in
-- Postgres — mirrors instances; SQLite is unscoped N=1.
CREATE TABLE placement_overrides (
    org_id             TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    key_kind           TEXT NOT NULL,
    key_value          TEXT NOT NULL,
    -- pinned_instance_id: pin this key to one instance regardless of the hash
    -- or that instance's liveness (the claim's aging tier still spills a
    -- pinned-but-dead owner's runs). NULL = no pin.
    pinned_instance_id TEXT,
    -- replicas: the hot-key K. The top-K rendezvous candidates all count as
    -- preferred, bounding a hot monorepo's cache to K instances. 0 = unset
    -- (single-owner default). Only consulted when pinned_instance_id IS NULL.
    replicas           INTEGER NOT NULL DEFAULT 0,
    updated_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, key_kind, key_value)
);

-- +goose Down
SELECT 'down not supported';
