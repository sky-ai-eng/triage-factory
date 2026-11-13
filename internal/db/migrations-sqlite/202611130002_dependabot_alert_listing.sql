-- +goose Up
-- The poll's listing of a repository's open Dependabot alerts, beside the
-- answer that says whether they can be read. alerts_listed_at is when the
-- poll last listed them in full, NULL until the repository's baseline listing
-- and again once no team tracks it; alerts_etag is the ETag GitHub answered
-- that listing with, '' when the listing took more than one page.
--
-- alerts_listed_at is written from Go as UTC, like checked_at.
ALTER TABLE repository_dependabot_access ADD COLUMN alerts_listed_at DATETIME;
ALTER TABLE repository_dependabot_access ADD COLUMN alerts_etag TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'down not supported';
