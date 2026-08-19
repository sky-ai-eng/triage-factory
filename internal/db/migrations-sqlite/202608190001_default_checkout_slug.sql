-- +goose Up
-- Rename the reserved default-checkout worktree ref from "@default" to
-- "default".
--
-- conversation_worktrees.ref is both the (conversation, repo, ref) PK
-- discriminator and the value `workspace add` looks a reservation up by
-- (worktree.CheckoutRefSlug). The binary now spells the no-selector form
-- "default", so a stored "@default" row would never be found by an idempotent
-- re-add again — a conversation resumed across the upgrade would re-clone a
-- repo it already holds into a second checkout directory. Rewriting the ref
-- keeps the reservation addressable.
--
-- path is deliberately NOT rewritten: it names the directory that actually
-- exists on disk (".../@default" for a checkout an older build created), and
-- the row's job is to point at that real directory. New reservations compute
-- their path from the new slug on their own.
--
-- No collision guard is needed on the PK: "default" was never a value this
-- column could previously hold ("@default" was the only no-selector spelling,
-- a --ref checkout slugs to "ref-<branch>", and a PR to "pr-<N>"), so the
-- rewrite cannot land on an existing (conversation_id, repository_id, ref).
UPDATE conversation_worktrees SET ref = 'default' WHERE ref = '@default';

-- +goose Down
SELECT 'down not supported';
