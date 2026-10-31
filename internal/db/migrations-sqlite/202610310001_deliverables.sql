-- +goose Up
-- The artifacts table becomes deliverables: the work a run prepared that a
-- person acts on. Rows, columns and keys are unchanged. Each index is
-- recreated under the table's name, and the one on conversation_id is named
-- for that column, as the Postgres baseline's is.
ALTER TABLE artifacts RENAME TO deliverables;

DROP INDEX idx_artifacts_dedup;
CREATE UNIQUE INDEX idx_deliverables_dedup ON deliverables (org_id, scope, dedup_key);

DROP INDEX idx_artifacts_run;
CREATE INDEX idx_deliverables_conversation ON deliverables (conversation_id);

DROP INDEX idx_artifacts_team_created;
CREATE INDEX idx_deliverables_team_created ON deliverables (team_id, created_at DESC);

DROP INDEX idx_artifacts_org_created;
CREATE INDEX idx_deliverables_org_created ON deliverables (org_id, created_at DESC);

DROP INDEX idx_artifacts_node_id;
CREATE INDEX idx_deliverables_node_id ON deliverables (org_id, scope, node_id) WHERE node_id IS NOT NULL;

DROP INDEX idx_artifacts_external_id;
CREATE INDEX idx_deliverables_external_id ON deliverables (org_id, scope, kind, external_id) WHERE external_id IS NOT NULL;

DROP INDEX idx_artifacts_entity;
CREATE INDEX idx_deliverables_entity ON deliverables (entity_id) WHERE entity_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
