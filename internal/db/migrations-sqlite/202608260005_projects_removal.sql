-- +goose Up
-- The "projects" concept is removed: the classifier that assigned entities
-- to projects, the projects UI, the entity panel + backfill, project
-- bundles, pinned repos, and spec authorship. See the epic. Only the
-- TF-internal `projects` table is affected — external Jira/Linear
-- "projects" (jira_project_status_rules, team_settings.jira_projects) are a
-- different concept and are untouched.

-- project_pinned_repos references projects(id); drop it first.
DROP INDEX project_pinned_repos_repository_idx;
DROP TABLE project_pinned_repos;

-- entities.project_id/classified_at/classification_rationale were the
-- classifier's assignment + rationale. The index must go first — SQLite
-- refuses to drop a column an index still references.
DROP INDEX idx_entities_project_id;
ALTER TABLE entities DROP COLUMN project_id;
ALTER TABLE entities DROP COLUMN classified_at;
ALTER TABLE entities DROP COLUMN classification_rationale;

DROP TABLE projects;

-- +goose Down
SELECT 'down not supported';
