-- +goose Up
-- The Overview's away line ("you were last here at 18:40 yesterday") needs an
-- anchor, and nothing stored one. It is written explicitly by the page rather
-- than stamped on read: a read that mutates is a read nobody can repeat, and
-- the caller that knows a HUMAN looked is the page, not the request log — a
-- rail-count refetch on an idle open tab keeps any last-action timestamp
-- perpetually fresh while nobody is there.
--
-- Nullable with no default, and NULL is load-bearing: it means this user has
-- never opened the Overview, which the page renders as its own thing (an
-- anchor at midnight, said out loud) rather than as a very old visit.
--
-- Keyed by user alone, like the row it lands on, so a multi-org user carries
-- one marker across their orgs. Accepted: the value is a display anchor read
-- at minute resolution, and per-org markers would be a second table to make
-- one sentence per org marginally more accurate.
ALTER TABLE user_settings ADD COLUMN overview_seen_at TIMESTAMP;

-- +goose Down
SELECT 'down not supported';
