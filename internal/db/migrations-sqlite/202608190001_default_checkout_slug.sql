-- +goose Up
-- Rename the reserved no-branch worktree ref from "@default" to "default".
--
-- conversation_worktrees.ref is both the (conversation_id, repository_id,
-- ref) PK discriminator and the value `workspace add` looks a reservation up
-- by (worktree.CheckoutRefSlug). The binary now resolves a bare add to the
-- repository row's base/default branch ("ref-<branch>") and spells the
-- fallback form — a row that names no branch — "default", so a stored
-- "@default" row is addressable again only under the new spelling. Rewriting
-- the ref keeps the fallback form findable by an idempotent re-add and keeps
-- `workspace list` on the live vocabulary.
--
-- path is deliberately NOT rewritten: it names the directory that actually
-- exists on disk (".../@default" for a checkout an older build created), and
-- the row's job is to point at that real directory. New reservations compute
-- their path from the new slug on their own.
--
-- No collision guard is needed on the PK: "default" was never a value this
-- column could previously hold ("@default" was the only no-branch spelling,
-- a --ref checkout slugs to "ref-<branch>", and a PR to "pr-<N>"), so the
-- rewrite cannot land on an existing (conversation_id, repository_id, ref).
UPDATE conversation_worktrees SET ref = 'default' WHERE ref = '@default';

-- +goose Down
SELECT 'down not supported';
