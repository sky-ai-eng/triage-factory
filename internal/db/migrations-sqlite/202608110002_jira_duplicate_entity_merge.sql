-- +goose Up

-- Merge the duplicate Jira entity rows that 202608100001 could not fold.
--
-- That migration folded non-canonical Jira keys onto the spelling Jira itself
-- answers with, and skipped every row whose canonical spelling was already
-- occupied: (source, source_id) is UNIQUE, folding onto a taken key raises,
-- and a migration that raises is a process that will not boot. It left two
-- shapes behind — a variant beside its healthy canonical twin ('sky-2' next to
-- 'SKY-2'), and two variants of one key with no canonical row at all ('sky-4'
-- and 'Sky-4'), where it elected the lowest id and folded only that one. Both
-- arrive here as the same shape, because electing a winner in the second case
-- is what minted its canonical row: a non-canonically keyed entity whose
-- canonical twin now exists.
--
-- Those rows are inert. The poller matches a refresh back to its entity by
-- exact source_id and Jira only ever answers with the canonical key, so a
-- variant can never match its own issue again — no diff, no event, and no task
-- will ever come off it. They are not silent either: the reachability
-- confirmation pass picks each one up as a candidate, asks Jira about it
-- directly, gets a 200 (a non-canonical key resolves perfectly well), and logs
-- that the issue resolves but no search returned it — once per grace period,
-- forever, naming an entity nothing in the product can act on.
--
-- SURVIVORSHIP: the canonically keyed row survives, always. This is the choice
-- 202608100001 declined to make, and it is not a coin flip. The variant's key
-- IS the defect; a merge that kept its id would keep a row that can never
-- match a refresh, which is the thing being repaired. So the direction is
-- forced, and the only real work is repointing what hangs off the loser.
--
-- WHAT MOVES is every row that references the variant's id: tasks, events,
-- event_queue, pending_firings, conversation_memory, conversation_memory_entities,
-- entity_links. Conversations come along with their task (they reach an entity
-- only through conversations.task_id), so they need no statement of their own.
-- Artifacts are a referent in spirit but not in schema — they carry no
-- entity_id, only the natural key in `target` — so an entity merge does not
-- reach them and this repair leaves them alone.
--
-- WHAT DOES NOT MOVE are the variant's own columns: snapshot, title,
-- description, project, owning-team override. Those describe the same issue
-- from a read that stopped happening a while ago, and the survivor is the row
-- the source still answers for. A column-level "best of both" would be a
-- heuristic with no rule behind it, so the survivor stays authoritative for
-- its own fields and the variant's are dropped with it.
--
-- The population is closed: since 202608100001's sibling code fix, every path
-- that mints a Jira entity normalizes at domain.EntityRefForExternal or reads
-- the key off Jira's own response, so no new variant can be written. This is a
-- one-time repair of a bounded, historical set, and it costs nothing on the
-- installs (most of them) where that set is empty.
--
-- SQLite only, like the fold it finishes. The Postgres tree is a single
-- fresh-install baseline for a mode that has not shipped, so there is no
-- deployed Postgres row anywhere to repair and a data repair has no place in a
-- schema document.

-- The pairing, resolved once and before anything moves. A plain staging table
-- rather than TEMP (the idiom 202607230001 already uses for its own staging),
-- dropped at the end of this migration and never seen by the app. The unique
-- index both makes the lookups below indexed and asserts the shape: a variant
-- has exactly one survivor, because it is matched on an exact UNIQUE key.
CREATE TABLE jira_entity_merge_src AS
SELECT variant.id  AS variant_id,
       survivor.id AS survivor_id
FROM entities variant
JOIN entities survivor
  ON survivor.source    = 'jira'
 AND survivor.source_id = upper(trim(variant.source_id))
WHERE variant.source = 'jira'
  AND variant.source_id <> upper(trim(variant.source_id));

CREATE UNIQUE INDEX jira_entity_merge_src_variant ON jira_entity_merge_src(variant_id);

-- tasks. The partial unique index on (entity_id, event_type, dedup_key) WHERE
-- status NOT IN ('done','dismissed') is the one constraint a repoint can trip,
-- so OR IGNORE moves everything that does not collide and leaves exactly the
-- collisions behind. Nothing else on this table can refuse the write: the
-- status and claim CHECKs are untouched by it, and the survivor satisfies the
-- entity_id FK by construction. A terminal task is outside the partial index
-- and therefore always moves here — which is what makes the mop-up below safe
-- to write unconditionally.
UPDATE OR IGNORE tasks
SET entity_id = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = tasks.entity_id)
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

-- What is left is an active task on a variant whose (event_type, dedup_key)
-- slot is already held by an active task on the survivor — the same situation
-- on the same issue, carded twice — or by another variant of the same key.
-- The dedup invariant allows one active task per slot and the survivor's is
-- the one that will go on receiving events, so the variant's closes as the
-- duplicate it is. Dismissed rather than deleted: its conversations, its
-- events and its history stay readable, and the row stays on the entity that
-- is actually tracked. Writing the status in the same statement as the
-- repoint is what makes the repoint legal — a dismissed row is outside the
-- partial index, so there is no slot left to collide over.
UPDATE tasks
SET entity_id    = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = tasks.entity_id),
    status       = 'dismissed',
    closed_at    = CURRENT_TIMESTAMP,
    close_reason = 'duplicate_entity_merged'
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

-- events. Repointing an append-only row is deliberate and narrow: the event's
-- own content — type, metadata, occurred_at, created_at — is untouched, and
-- both rows always named the same real issue. The alternative to moving the
-- log is discarding it.
UPDATE events
SET entity_id = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = events.entity_id)
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

UPDATE event_queue
SET entity_id = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = event_queue.entity_id)
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

UPDATE pending_firings
SET entity_id = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = pending_firings.entity_id)
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

-- conversation_memory. One memory row per conversation (UNIQUE(conversation_id)),
-- nothing keyed on the entity, so every row moves.
UPDATE conversation_memory
SET entity_id = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = conversation_memory.entity_id)
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

-- conversation_memory_entities is keyed (conversation_id, entity_id), so a
-- conversation that touched both spellings collides. The leftover is a second
-- link from one conversation to one issue and is simply redundant, so it goes.
-- Its `role` may differ from the surviving link's ('produced' vs 'touched');
-- the survivor's wins, which is the same rule applied everywhere else here.
UPDATE OR IGNORE conversation_memory_entities
SET entity_id = (SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = conversation_memory_entities.entity_id)
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

DELETE FROM conversation_memory_entities
WHERE entity_id IN (SELECT variant_id FROM jira_entity_merge_src);

-- entity_links resolves on both endpoints, so it has a shape the other tables
-- do not: a link between a variant and its own survivor becomes a link from
-- the issue to itself. The PK (from, to, kind) would happily store that, so it
-- is deleted before the repoint rather than left to be found later.
DELETE FROM entity_links
WHERE (from_entity_id IN (SELECT variant_id FROM jira_entity_merge_src)
    OR to_entity_id   IN (SELECT variant_id FROM jira_entity_merge_src))
  AND COALESCE((SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = from_entity_id), from_entity_id)
    = COALESCE((SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = to_entity_id),   to_entity_id);

-- Both endpoints in one statement: SQLite evaluates every SET expression
-- against the pre-update row, so a link whose two ends are both variants
-- resolves correctly instead of half-moving. OR IGNORE for the case where the
-- resolved link already exists.
UPDATE OR IGNORE entity_links
SET from_entity_id = COALESCE((SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = from_entity_id), from_entity_id),
    to_entity_id   = COALESCE((SELECT survivor_id FROM jira_entity_merge_src WHERE variant_id = to_entity_id),   to_entity_id)
WHERE from_entity_id IN (SELECT variant_id FROM jira_entity_merge_src)
   OR to_entity_id   IN (SELECT variant_id FROM jira_entity_merge_src);

-- Whatever still points at a variant lost to an identical link that already
-- existed on the survivor, and is redundant.
DELETE FROM entity_links
WHERE from_entity_id IN (SELECT variant_id FROM jira_entity_merge_src)
   OR to_entity_id   IN (SELECT variant_id FROM jira_entity_merge_src);

-- Nothing references the variants now.
--
-- This DELETE is only half a check on the list above, and it is worth being
-- precise about which half. Foreign keys are enforced on the application's
-- connection, so a referent missed in events / event_queue / tasks /
-- conversation_memory raises here and fails the migration — loud, and at boot.
-- The other three (pending_firings, conversation_memory_entities,
-- entity_links) are ON DELETE CASCADE, so a repoint missed there is not caught
-- at all: the row is deleted along with the variant, quietly. The test seeds
-- every one of the seven for that reason — the CASCADE ones cannot rely on
-- this statement to notice.
DELETE FROM entities
WHERE id IN (SELECT variant_id FROM jira_entity_merge_src);

DROP TABLE jira_entity_merge_src;

-- +goose Down
SELECT 'down not supported';
