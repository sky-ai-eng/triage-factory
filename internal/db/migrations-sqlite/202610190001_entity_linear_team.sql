-- +goose Up
-- entities.linear_team_id: the UUID of the Linear team a Linear issue sits in,
-- the id linear_team_rules is keyed by. It is a column rather than a field read
-- out of snapshot_json because turning a source off clears the snapshot, and
-- an issue's team has to survive that: the belt places the issue by it, and an
-- issue that goes unreachable while the source is off names it for the
-- router's team gate. The column is set at creation from the response that
-- found the issue and moved by every snapshot write to the snapshot's team,
-- and clearing the snapshot leaves it.
--
-- Required for a Linear row and NULL for every other source, enforced by the
-- store rather than a CHECK, as on the other ADD COLUMN text columns: adding a
-- CHECK means a full table rebuild. The backfill reads the team from each
-- row's stored snapshot. A Linear row with no snapshot to read it from (a
-- stub no poll has refreshed yet, or a row a source pause cleared) stays NULL
-- until the tracker's next snapshot write of it.
ALTER TABLE entities ADD COLUMN linear_team_id TEXT;

UPDATE entities
   SET linear_team_id = json_extract(snapshot_json, '$.team_id')
 WHERE source = 'linear'
   AND json_valid(snapshot_json)
   AND COALESCE(json_extract(snapshot_json, '$.team_id'), '') <> '';

-- +goose Down
SELECT 'down not supported';
