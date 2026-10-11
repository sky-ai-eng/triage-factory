-- +goose Up
-- claims.source_manifest: an engagement's source manifest — for each event
-- source, whether its credential was sealed into the engagement's bundle and,
-- when not, why not. Multi mode's credential provisioner writes it on every
-- seal. Local mode seals no bundles, so nothing writes it here; the column
-- keeps the claims projection both dialects read one shape. NULL = never
-- sealed.
ALTER TABLE claims ADD COLUMN source_manifest TEXT;

-- +goose Down
SELECT 'down not supported';
