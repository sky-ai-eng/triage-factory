-- +goose Up
-- Token breakdown on runs + curator_requests (TFAC-473). The cache-token
-- breakdown (input / output / cache_read / cache_creation) is already captured
-- for system jobs (system_llm_runs, TFAC-451) but was missing for the two
-- biggest spend categories: delegated runs and curator turns. Adding it here
-- lets cache-rate analysis work across *all* spend and lets TFAC-472's
-- llm_spend view expose tokens natively for every source (dropping its `0`
-- placeholders for runs/curator). Mirrors system_llm_runs' four columns.
--
-- Forward-only per the SQLite (shipped) migration rule — never edit the
-- baseline; the Postgres baseline (unreleased) carries these columns directly.
--
-- Both tables populate going forward by SETting each column to the absolute
-- SUM over the owning message table (run_messages for runs; curator_messages
-- for curator turns) — idempotent, since the SUM is always the full current
-- total. EVERY terminal write does this roll-up, not just normal completion:
-- the streaming sink writes per-message tokens as the turn runs, so a run or
-- request that ends by cancel or infra-failure has real token rows that must
-- be reflected rather than stranded at 0. So the SUM lives in every terminal
-- write: normal completion (Complete / CompleteRequest), cancel + infra-fail
-- (MarkCancelledIfActive / MarkFailedIfActive / MarkRequestCancelledIfActive),
-- and the boot-time orphan sweeps (ReconcileOrphanedRuns /
-- CancelOrphanedNonTerminalRequests). MarkDiscarded is the lone exception — it
-- only acts on pending_approval rows, already rolled up at completion. Net:
-- these columns are trustworthy for every terminal state, so the TFAC-472 spend
-- view reads them natively without joining the message tables.
ALTER TABLE runs ADD COLUMN input_tokens          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN output_tokens         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN cache_read_tokens     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;

ALTER TABLE curator_requests ADD COLUMN input_tokens          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE curator_requests ADD COLUMN output_tokens         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE curator_requests ADD COLUMN cache_read_tokens     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE curator_requests ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;

-- Backfill both tables' history from the per-message tokens already on disk:
-- runs from run_messages, curator_requests from curator_messages (both message
-- tables carry all four token columns, populated from the stream sink since
-- before this ticket). The forward-write path SETs the same SUM at completion,
-- so this just retrofits rows that completed before this migration — recovering
-- historical curator spend for free, not just runs. (No Postgres backfill —
-- that baseline is fresh.)
UPDATE runs SET
  input_tokens          = COALESCE((SELECT SUM(input_tokens)          FROM run_messages WHERE run_id = runs.id), 0),
  output_tokens         = COALESCE((SELECT SUM(output_tokens)         FROM run_messages WHERE run_id = runs.id), 0),
  cache_read_tokens     = COALESCE((SELECT SUM(cache_read_tokens)     FROM run_messages WHERE run_id = runs.id), 0),
  cache_creation_tokens = COALESCE((SELECT SUM(cache_creation_tokens) FROM run_messages WHERE run_id = runs.id), 0);

UPDATE curator_requests SET
  input_tokens          = COALESCE((SELECT SUM(input_tokens)          FROM curator_messages WHERE request_id = curator_requests.id), 0),
  output_tokens         = COALESCE((SELECT SUM(output_tokens)         FROM curator_messages WHERE request_id = curator_requests.id), 0),
  cache_read_tokens     = COALESCE((SELECT SUM(cache_read_tokens)     FROM curator_messages WHERE request_id = curator_requests.id), 0),
  cache_creation_tokens = COALESCE((SELECT SUM(cache_creation_tokens) FROM curator_messages WHERE request_id = curator_requests.id), 0);

-- +goose Down
SELECT 'down not supported';
