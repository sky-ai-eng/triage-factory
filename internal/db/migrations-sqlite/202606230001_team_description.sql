-- +goose Up
-- Team rename/description (TFAC-446): the SQLite baseline's teams table has
-- name + slug but no description column (the Postgres baseline already carries
-- it). Add it so TeamsStore.Update has the same shape on both backends — the
-- store + handler are multi-mode (Postgres) in production, but the SQLite impl
-- and its parity tests exercise the same write. Nullable, no default: an unset
-- description reads back as the empty string via COALESCE.
ALTER TABLE teams ADD COLUMN description TEXT;

-- +goose Down
SELECT 'down not supported';
