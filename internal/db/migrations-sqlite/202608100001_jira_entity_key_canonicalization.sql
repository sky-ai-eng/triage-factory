-- +goose Up

-- Fold tracked Jira entity keys to the canonical spelling Jira answers with.
--
-- Jira resolves an issue key case-insensitively everywhere — the REST issue
-- endpoints and JQL alike — but always answers with the canonical key. Entities
-- minted from a caller-supplied key (an agent's exec verb, an artifact target)
-- therefore recorded whatever spelling the caller used, while the poller matches
-- a refresh back to its entity by exact source_id. A row whose key differed in
-- case from the canonical one could never match its own issue again: the refresh
-- answered under the canonical spelling, the lookup missed, and the entity held
-- its last snapshot indefinitely. Nothing errored at any point, which is why
-- these rows accumulate silently rather than surfacing as failures.
--
-- The code path that minted them now normalizes at the boundary
-- (domain.EntityRefForExternal), so this is a one-time repair of rows already
-- written, not an ongoing correction.
--
-- Rows whose canonical spelling is already taken are deliberately left alone.
-- That row is a duplicate of a healthy entity — the same issue tracked twice,
-- once under each spelling — and (source, source_id) is UNIQUE, so folding it
-- would collide. Merging the two is not a rename: tasks, events, conversations,
-- and memory links hang off the entity id, and choosing which id survives is a
-- decision with consequences this migration has no standing to make. The
-- untouched row keeps everything it references, its healthy twin carries the
-- live tracking, and the poller's confirmation pass reports it rather than
-- leaving it silent.
--
-- The `id = (SELECT min(...))` clause handles the same collision between two
-- rows that are BOTH non-canonical — 'sky-4' and 'Sky-4' with no 'SKY-4' — which
-- the NOT EXISTS above cannot see, because at statement start neither row
-- occupies the spelling they would both fold onto. Left to the NOT EXISTS alone
-- this statement fails outright on the UNIQUE constraint (verified, not
-- theorised: see the migration test's two-variant case), and a migration that
-- fails is a process that will not boot. Electing one winner per canonical key
-- makes the outcome independent of how the engine interleaves its scan with its
-- writes; the losers are duplicates and get the same treatment as any other —
-- left intact for a human.
UPDATE entities
SET source_id = upper(trim(source_id))
WHERE source = 'jira'
  AND source_id <> upper(trim(source_id))
  AND NOT EXISTS (
        SELECT 1
        FROM entities twin
        WHERE twin.source = 'jira'
          AND twin.source_id = upper(trim(entities.source_id))
  )
  AND id = (
        SELECT min(pick.id)
        FROM entities pick
        WHERE pick.source = 'jira'
          AND upper(trim(pick.source_id)) = upper(trim(entities.source_id))
  );

-- +goose Down
SELECT 'down not supported';
