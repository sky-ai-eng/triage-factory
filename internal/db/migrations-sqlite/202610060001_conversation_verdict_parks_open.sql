-- +goose Up
-- A conversation no longer concludes. Its status says whether the transcript
-- can be driven (queued, running, open, or failed when the runtime under it
-- died), and whether the work is done is the blueprint run's to say. A step's
-- verdict (outcome, outcome_reason, result_summary) stays on the step's
-- conversation, because the conversation is the step instance, but recording
-- it parks the row `open` like any other turn end.
--
-- So 'completed' leaves the stored vocabulary, and the rows carrying it are
-- rewritten rather than taught to every predicate: those predicates are
-- exclusions (`status NOT IN (…)`), and a retired value left out of one
-- readmits a concluded conversation instead of failing closed. Every row this
-- touches keeps its verdict columns untouched, and its blueprint run already
-- holds the terminal the verdict produced (or has advanced past the step), so
-- `open` with a verdict under a blueprint that no longer drives it is the
-- faithful new spelling of what happened. Migrated rows are indistinguishable
-- from a verdict recorded by this build.
--
-- completed_at stays as the terminal write stamped it: it is the conclusion
-- stamp that makes an `open` row concluded, and what the idle sweeps age the
-- row from, exactly as they did while it read `completed`. park_reason is
-- cleared: a verdict records no reason, because nothing stopped the
-- conversation, and a reason left over from an earlier park in the same
-- conversation would describe a turn that is no longer the last one.
UPDATE conversations
SET status = 'open',
    park_reason = NULL
WHERE status = 'completed';

-- +goose Down
SELECT 'down not supported';
