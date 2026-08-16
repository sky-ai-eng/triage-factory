-- +goose Up
-- current_url: where the object an audited action touched lives NOW, when that
-- differs from where it lived at the time of the act. NULL = never moved.
--
-- The ledger's contract splits in two here, deliberately. The record of the
-- act — target, action, detail_json, credential, occurred_at, and url as
-- captured — is immutable, exactly as before. The POINTER to the object is
-- maintained: a repository rename fills this column (in the same transaction
-- as the rest of its rewrite) and reads serve COALESCE(current_url, url), so
-- the action feed's link keeps resolving to the right object after the freed
-- name is re-claimed by a stranger, while the stored history stays
-- byte-identical. This is the only column on the table any UPDATE may touch.
ALTER TABLE external_actions ADD COLUMN current_url TEXT;

-- +goose Down
SELECT 'down not supported';
