-- +goose Up
-- idx_artifacts_org_created backs the org-wide, newest-first scan the
-- bot-activity audit feed runs (ArtifactStore.ListByOrgSystem, TFAC-483).
-- The existing idx_artifacts_team_created covers the team-scoped feed
-- (team_id, created_at DESC); this is its org-grain sibling so the org feed's
-- ORDER BY created_at DESC over the whole org doesn't fall back to a full scan.
-- Mirrors the Postgres baseline's idx_artifacts_org_created.
CREATE INDEX idx_artifacts_org_created ON artifacts (org_id, created_at DESC);

-- +goose Down
SELECT 'down not supported';
