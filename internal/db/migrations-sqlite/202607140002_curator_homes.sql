-- +goose Up
-- Curator session homing (spec §6.3): a curator session is the one *stateful*
-- placement client — sticky per-project cwd + shared-RO worktrees per
-- (org, repo). At N>1 an unplaced session re-materializes every hot project's
-- pinned repos on every pod (storage ×N) AND a second control pod can mint a
-- second live session for the same project (interleaved streams, duplicated
-- sandboxes). Homing fixes both structurally: the session lives at exactly one
-- home executor, minted on first turn from the capacity-weighted rendezvous
-- hash, and every control pod routes turns there.
--
-- Both additions are no-ops in local mode (N=1: the one process is always its
-- own home). They land here for store-interface + conformance symmetry across
-- dialects, the same rule instances / placement / run_credentials followed.

-- curator_requests.home_instance_id: the instance that executes this turn.
-- Stamped at creation from the resolved home (a remote executor when the
-- serving control pod forwards; the serving pod itself in the local /
-- no-executor-available fallback). NULL only for the untouched local / role=all
-- path, which never forwards and sweeps globally. The executor claim loop reads
-- it (WHERE home_instance_id = me AND status = 'queued'); the ownership-scoped
-- boot sweep + the leader reaper read it to retire turns stranded on a pod that
-- died.
ALTER TABLE curator_requests ADD COLUMN home_instance_id TEXT;

-- Partial index for the executor claim loop's hot path: pull my queued turns.
-- Mirrors idx_runs_queued_preferred's shape (the run queue's tier-1 claim).
CREATE INDEX idx_curator_requests_homed_queued
    ON curator_requests (home_instance_id, created_at, id)
    WHERE status = 'queued';

-- curator_homes: the durable (org, project) -> home executor map. Minted on the
-- first turn from the rendezvous winner over live executors; kept sticky while
-- the home's heartbeat stays fresh (the hash gives the initial placement, this
-- row pins it until death, so a benign fleet reshuffle never thrashes a warm
-- cache); re-minted by the next turn once the home goes heartbeat-stale
-- (re-home on death — the new executor re-materializes worktrees via
-- seed-on-demand, one cold blobless clone, rare and self-healing).
--
-- Keyed (org_id, project_id). home_boot_epoch snapshots the home instance's
-- registration epoch at mint time so a stale re-home target (same id, newer
-- boot) is legible in the row. Admin-pool-only / system table, same posture as
-- instances / placement_overrides: pure placement coordination, never a
-- browsable RLS surface. SQLite is unscoped N=1 — the row is inert (the hash
-- always returns self) but exists for cross-dialect symmetry.
CREATE TABLE curator_homes (
    org_id           TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    project_id       TEXT NOT NULL,
    home_instance_id TEXT NOT NULL,
    home_boot_epoch  INTEGER NOT NULL DEFAULT 0,
    homed_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, project_id)
);

-- +goose Down
SELECT 'down not supported';
