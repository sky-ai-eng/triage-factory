-- +goose Up
-- A person's claim mints the conversation they work the task in. Tasks a
-- person already holds were claimed with no such write, so each open one with
-- no un-ended top-level conversation gets the row the claim would have
-- minted: an interactive conversation on the SDK runtime, manual with the
-- claimant as its creator, team-visible on the local team, and parked with no
-- verdict (status 'open' and parked_at set, with no park reason, completion,
-- or outcome), so it reads as live and nothing drives it until a person sends
-- a message.
INSERT INTO conversations (
    id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
    trigger_type, trigger_id, origin, team_id, visibility, creator_user_id,
    actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
    status, parked_at, park_reason, completed_at, outcome, outcome_reason,
    queued_at)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       t.org_id, 'interactive', 'sdk', t.id, NULL, NULL, NULL,
       'manual', NULL, 'interactive', '00000000-0000-0000-0000-000000000010', 'team', t.claimed_by_user_id,
       NULL, NULL, NULL, NULL,
       'open', CURRENT_TIMESTAMP, NULL, NULL, NULL, NULL,
       NULL
  FROM tasks t
 WHERE t.status NOT IN ('done', 'dismissed')
   AND t.claimed_by_user_id IS NOT NULL
   AND NOT EXISTS (
       SELECT 1 FROM conversations live
        WHERE live.task_id = t.id
          AND live.ended_at IS NULL AND live.parent_conversation_id IS NULL);

-- +goose Down
SELECT 'down not supported';
