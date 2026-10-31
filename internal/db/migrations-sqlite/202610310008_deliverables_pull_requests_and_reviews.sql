-- +goose Up
-- A deliverable is a pull request or a review an agent prepared for a person.
-- Every other kind (branch, issue, comment, message) recorded what a run
-- changed, which the external-action ledger now records on its own, so those
-- rows go. Ledger rows recorded before they named their object have no
-- artifact to stand in for the rows deleted here; that loss is accepted.
-- There is no CHECK on kind: SQLite adds no table constraint to an existing
-- table without rebuilding it, and the store refuses any other kind.
DELETE FROM deliverables WHERE kind NOT IN ('pull_request', 'review');

-- +goose Down
SELECT 'down not supported';
