-- +goose Up
-- The workspace key becomes the task id. A delegated run's tree, its snapshot
-- blob and this lifecycle row were keyed by the blueprint run, so every
-- re-delegation on a task minted a new key and cloned a fresh checkout while
-- the previous run's blob sat orphaned. One task, one workspace: the next
-- conversation on a task picks the tree up where the last one left it.
--
-- The data step picks a winner per task before the key narrows. A task may
-- hold rows for several of its runs and the new primary key admits one, so the
-- surviving row is the one belonging to the task's most recently started run —
-- the only one whose blob is the state anybody would want continued. Ties
-- break on the run id so the choice is total. The blobs those rows point at
-- are moved to the task key by the local boot step beside the worktree sweep;
-- a row whose blueprint run is already gone (the old FK cascaded) cannot name
-- a task and is dropped with it.
CREATE TABLE workspace_snapshots_by_task (
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    task_id         TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    state           TEXT NOT NULL CHECK (state IN ('pending', 'written', 'failed')),
    writer_claim_id TEXT NOT NULL,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, task_id)
);

INSERT INTO workspace_snapshots_by_task (org_id, task_id, state, writer_claim_id, updated_at)
SELECT org_id, task_id, state, writer_claim_id, updated_at
FROM (
    SELECT ws.org_id          AS org_id,
           br.task_id         AS task_id,
           ws.state           AS state,
           ws.writer_claim_id AS writer_claim_id,
           ws.updated_at      AS updated_at,
           ROW_NUMBER() OVER (
               PARTITION BY ws.org_id, br.task_id
               ORDER BY br.started_at DESC, br.id DESC
           ) AS rn
    FROM workspace_snapshots ws
    JOIN blueprint_runs br ON br.id = ws.blueprint_run_id
)
WHERE rn = 1;

DROP TABLE workspace_snapshots;
ALTER TABLE workspace_snapshots_by_task RENAME TO workspace_snapshots;

-- +goose Down
SELECT 'down not supported';
