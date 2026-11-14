-- +goose Up
-- When the poll last read what Dependabot did about a repository's open
-- alerts (the pull request it opened, or the error it reported), which the
-- alert listing does not carry. NULL until the first read. Written from Go as
-- UTC, like checked_at.
ALTER TABLE repository_dependabot_access ADD COLUMN updates_read_at DATETIME;

-- +goose Down
SELECT 'down not supported';
