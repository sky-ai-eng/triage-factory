-- +goose Up
-- The ETag GitHub answered the poll's last listing of a repository's open
-- Dependabot alerts with, beside the answer that says whether they can be
-- read: the next listing's condition, '' when there is none to send.
ALTER TABLE repository_dependabot_access ADD COLUMN alerts_etag TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'down not supported';
