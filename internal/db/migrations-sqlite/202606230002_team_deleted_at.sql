-- +goose Up
-- Team archive/restore (TFAC-448): the SQLite baseline's teams table has no
-- deleted_at column (the Postgres baseline carries it). Add it for store +
-- read-filter parity — archive/restore is a multi-mode (Postgres) feature in
-- production, but the SQLite impl and its parity tests exercise the same
-- soft-delete read filtering. Nullable, no default: NULL = active, a non-NULL
-- timestamp = archived.
ALTER TABLE teams ADD COLUMN deleted_at TIMESTAMP;

-- +goose Down
SELECT 'down not supported';
