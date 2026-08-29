-- +goose Up
-- "Mine" on the pull-request list grows a second leg: authored by my login,
-- OR commissioned by me. The first leg reads the snapshot's author, which for
-- a PR TF opened is a bot that maps to no TF user — so a run the viewer asked
-- for produces work that never appears in their own list.
--
-- The answer is stamped at the exec recording funnel, beside owning_team_id
-- (the team half of the same moment), from the conversation's creator: the
-- human who pressed delegate or swiped agent. Non-empty exactly for manual
-- conversations; an event-triggered run leaves it NULL, because nobody asked.
--
-- It is provenance, not ownership. owning_team_id is the structural-owner axis
-- author-centric routing reads; this column answers "who asked", and must not
-- be read — or spelled — as a second owner.
--
-- Entity-level rather than derived through conversations at read time, because
-- artifacts.conversation_id is ON DELETE SET NULL: a join loses attribution at
-- exactly the moment old conversations are purged. The entity outlives
-- everything else in the chain and carries the answer itself.
ALTER TABLE entities ADD COLUMN commissioned_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;

-- Serves the list's second leg, which is OR-ed with the author predicate
-- idx_entities_github_author serves. Partial on the column being set: a stamp
-- only ever lands on a bot-opened pull request, and every other entity in the
-- install would otherwise be indexed to answer a question about none of them.
CREATE INDEX IF NOT EXISTS idx_entities_commissioned_by
    ON entities (commissioned_by_user_id)
    WHERE commissioned_by_user_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
