-- +goose Up
-- Index the PR author the dashboard's "your open PRs" list filters on.
--
-- That read used to select every GitHub entity row and drop the non-matches in
-- Go, so the work it did was proportional to the org's whole PR history rather
-- than to the caller's own PRs — and it could not be paged, because a LIMIT
-- applied before a Go-side filter windows the wrong set (page 2 comes back
-- empty while page 3 has rows). The filter is now a SQL predicate on
-- json_extract(snapshot_json, '$.author'), and this is the expression index
-- that serves it.
--
-- Day one: local mode is a single user's machine, so the entity count here is
-- small enough that the scan was survivable — this index is not what makes the
-- read correct (the SQL predicate is), it is what keeps it cheap as a
-- long-lived local install accumulates history. The partial predicate matches
-- the query's own, so rows with no snapshot cost nothing to keep indexed.
--
-- json_valid() is in the predicate, and it is load-bearing rather than
-- decorative. This dialect stores the snapshot as TEXT, so an unparseable
-- value can exist in the column (the reader already skips one rather than
-- failing the whole panel). Without the guard, json_extract() would raise
-- "malformed JSON" — at INDEX-MAINTENANCE time, which would turn writing a bad
-- snapshot into a failed write, and at query time, which would turn one bad row
-- into a 500 for the entire dashboard. The query carries the same guard ahead
-- of the extract, so the two predicates match and the index is still used.
CREATE INDEX IF NOT EXISTS idx_entities_github_author
    ON entities (json_extract(snapshot_json, '$.author'))
    WHERE source = 'github' AND snapshot_json IS NOT NULL AND snapshot_json != ''
      AND json_valid(snapshot_json);

-- +goose Down
SELECT 'down not supported';
