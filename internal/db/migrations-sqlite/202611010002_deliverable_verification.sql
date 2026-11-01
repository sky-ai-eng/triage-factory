-- +goose Up
-- deliverables.verification: whether the recording run is known to have opened
-- the pull request. 'pending' on a pull request create's row until the
-- reconciler settles it from GitHub's answer: 'verified', or 'foreign' when
-- GitHub shows the run did not open it, which also closes a draft or open
-- row. Only a verified row is acted on through TF. Every other writer's row, and every row
-- already stored, is 'verified'. App-validated: ALTER TABLE cannot add the
-- CHECK the Postgres column carries.
ALTER TABLE deliverables ADD COLUMN verification TEXT NOT NULL DEFAULT 'verified';

-- +goose Down
SELECT 'down not supported';
