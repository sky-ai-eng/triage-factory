-- +goose Up
-- conversations gains `system_block`: this conversation's own system block —
-- the run's facts, its verb reference, its mission and its step addendum — as
-- the launch composed it.
--
-- It is a fourth SDK resume coordinate, beside sdk_session_id, worktree_path
-- and model. The SDK harness is handed its system append once, at launch, and
-- replays only the session transcript afterwards, so a resumed turn has to be
-- handed the same string again. Recomposing it there would read today's prompt
-- row and today's team settings and wake the conversation on a mission it was
-- never launched with, which is the one thing the append must not do.
--
-- Block 1 is deliberately NOT stored: agentprompt.Build is byte-identical for a
-- fixed spec — the cacheable-prefix property the whole prompt package is built
-- around — so the resume rebuilds it rather than keeping a copy per row.
--
-- Only an SDK engagement writes it, so in practice only local mode does: a
-- multi delegation mints `native`, and the native loop sends its block on every
-- request and needs nothing stored. The column exists in both dialects because
-- the store interface is dual-dialect.
--
-- Empty is a legal value rather than a missing one. Every section of the block
-- is optional, so a conversation with nothing to say in any of them composes
-- the empty string, and a resume that reads it back sends the framework blocks
-- alone.
ALTER TABLE conversations ADD COLUMN system_block TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'down not supported';
