-- +goose Up
-- A later transcript position the written blob is known to reflect: a
-- checkpoint that found the tree unchanged records it instead of uploading the
-- same tree again, so the position inside the blob's manifest can be older
-- than what the blob covers. covered_fingerprint is the tree it describes,
-- which a restore matches against the manifest before trusting it. Both NULL
-- until such a checkpoint, and cleared by every begin. No backfill: a blob
-- written before the upgrade restores to its manifest's position.
ALTER TABLE workspace_snapshots ADD COLUMN covered_position REAL;
ALTER TABLE workspace_snapshots ADD COLUMN covered_fingerprint TEXT;
-- +goose Down
SELECT 'down not supported';
