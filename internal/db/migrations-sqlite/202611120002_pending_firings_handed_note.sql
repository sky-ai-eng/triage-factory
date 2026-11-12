-- +goose Up
-- handed_note_id names the staged note a firing's event was handed to a
-- conversation as (the messages row id, in decimal), while the firing's task
-- had a blueprint running and one of its conversations started an
-- engagement. NULL on a firing nothing was handed over for. The firing stays
-- unsettled: the firing worker settles it once the run ends, by whether the
-- note was read.
ALTER TABLE pending_firings ADD COLUMN handed_note_id TEXT NULL;

-- +goose Down
SELECT 'down not supported';
