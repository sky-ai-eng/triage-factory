-- +goose Up
-- The last tip of each base branch the poll's branch watch processed: one row
-- per (repository, branch) an open pull request targets, written only by the
-- watch. tip_sha is the tip whose move has been recorded on the rows of the
-- pull requests it can have affected (base_move_reason below), etag the ref
-- read's from its last 200, and observed_at when the tip was last checked, a
-- 304 included. Stored so a restart does not take
-- every base branch for newly seen and probe every pull request on it.
CREATE TABLE branch_tips (
    repository_id TEXT NOT NULL,
    branch        TEXT NOT NULL,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    tip_sha       TEXT NOT NULL,
    etag          TEXT NULL,
    observed_at   DATETIME NOT NULL,
    PRIMARY KEY (repository_id, branch),
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

-- The base branch the probe worker's last settling read was against, beside
-- the head it was read at: a retargeted pull request's answer is about a
-- branch it no longer merges into.
ALTER TABLE pull_request_mergeability ADD COLUMN mergeable_base_ref TEXT NULL;

-- A row's answer was read against the base its entity's snapshot names,
-- which is the best record of it there is. A row this leaves NULL is not
-- probed on its base until its next read records one.
UPDATE pull_request_mergeability
SET mergeable_base_ref = (
    SELECT NULLIF(json_extract(e.snapshot_json, '$.base_ref'), '')
    FROM entities e
    WHERE e.id = pull_request_mergeability.entity_id AND json_valid(e.snapshot_json)
);

-- A pull request's changed paths, written only by the branch watch, which
-- reads them when the pull request's base branch moves to decide whether the
-- move can have changed its mergeability. paths_json is a JSON array of
-- {"path", "change", "previous"}; paths_head_sha and paths_base_ref the head
-- and base the list was read at; paths_complete whether it holds every
-- changed file. The probe worker's columns are untouched by the watch, and
-- the watch's by the worker.
ALTER TABLE pull_request_mergeability ADD COLUMN paths_json TEXT NULL;
ALTER TABLE pull_request_mergeability ADD COLUMN paths_head_sha TEXT NULL;
ALTER TABLE pull_request_mergeability ADD COLUMN paths_base_ref TEXT NULL;
ALTER TABLE pull_request_mergeability ADD COLUMN paths_complete BOOLEAN NOT NULL DEFAULT 0;

-- A base branch move the stored answer may predate, written by the branch
-- watch for each pull request the move can have affected: base_move_reason is
-- base_moved or base_reset, and base_moved_at when the watch wrote it, in the
-- probe queue's leased_at shape. The poll admits a probe for it every cycle
-- until a probe leased at or after base_moved_at settles a read, which clears
-- both; a probe dropped or read before the move leaves them.
ALTER TABLE pull_request_mergeability ADD COLUMN base_move_reason TEXT NULL CHECK (base_move_reason IN ('base_moved','base_reset'));
ALTER TABLE pull_request_mergeability ADD COLUMN base_moved_at TEXT NULL;

-- +goose Down
SELECT 'down not supported';
