-- +goose Up
-- A blueprint run records the predicate of the trigger that fired it, and the
-- trigger's breaker counts a run only while the trigger still holds that
-- predicate, compared as text. A person's run, and a run with no trigger,
-- records ''.
--
-- Stored handlers can hold {"author_id_in":[]} or {"assignee_in":[]}, the
-- form shipped handlers were seeded with. Both match everything, and their
-- canonical form is NULL, so a save of the same predicate would change its
-- text and start its breaker over. Rows holding either, shipped or not, are
-- rewritten to NULL before the backfill reads them. Only the event types the
-- shipped handlers name are rewritten: each has the field, so the empty list
-- matches everything there, while the matcher refuses the field on a type
-- without it.
--
-- Every existing event-fired run then records its trigger's predicate as it
-- stands, so a streak that holds before the upgrade holds after it.

ALTER TABLE blueprint_runs ADD COLUMN scope_predicate_json TEXT NOT NULL DEFAULT '';

UPDATE event_handlers
   SET scope_predicate_json = NULL
 WHERE json_valid(scope_predicate_json)
   AND ((json(scope_predicate_json) = '{"author_id_in":[]}'
         AND event_type IN ('github:pr:ci_check_failed', 'github:pr:review_changes_requested',
                            'github:pr:review_commented', 'github:pr:conflicts'))
     OR (json(scope_predicate_json) = '{"assignee_in":[]}'
         AND event_type IN ('jira:issue:assigned', 'jira:issue:became_atomic',
                            'linear:issue:assigned', 'linear:issue:became_atomic')));

UPDATE blueprint_runs
   SET scope_predicate_json = COALESCE(
         (SELECT h.scope_predicate_json FROM event_handlers h WHERE h.id = blueprint_runs.trigger_id), '')
 WHERE trigger_type = 'event' AND trigger_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
