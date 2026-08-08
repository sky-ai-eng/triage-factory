-- +goose Up
-- The post-scoring re-derive is what actually fires a deferred
-- (min_autonomy_suitability > 0) trigger: the event-time pass skips those
-- triggers on the promise that the re-derive picks them up once a score
-- exists. That promise was only as durable as the process — the re-derive
-- runs as a post-commit callback, so a crash between the scores committing
-- and the callback finishing left the task 'scored' (no future cycle
-- revisits it) with its deferred triggers never evaluated.
--
-- rederive_owed makes the obligation a row. It is written in the same
-- statement that writes the scores, so the debt and the scores are never
-- separable, and cleared only by a re-derive pass that actually evaluated
-- the task. The scorer drains the owed set at cycle start, which is the
-- crash backstop behind the callback's fast path.
--
-- Exactly two writers: the scores write sets it, the re-derive clears it.
-- Nothing else touches it — a task may legitimately be pending for a
-- re-score while still owing a re-derive from the last one.
--
-- The default covers every pre-existing row: they are not owed a re-derive,
-- because either their re-derive already ran or they were never scored.
ALTER TABLE tasks ADD COLUMN rederive_owed BOOLEAN NOT NULL DEFAULT 0;

-- Partial index: the drain reads this set once per scoring cycle and it is
-- empty in every crash-free cycle, so the index spans only the rare owed
-- rows rather than the whole board. Keyed on created_at because the drain
-- reads the set oldest-first — an index on id alone would satisfy the
-- predicate and then sort, which is exactly the cost a backlog can't afford.
CREATE INDEX IF NOT EXISTS idx_tasks_rederive_owed ON tasks(created_at) WHERE rederive_owed = 1;

-- +goose Down
SELECT 'down not supported';
