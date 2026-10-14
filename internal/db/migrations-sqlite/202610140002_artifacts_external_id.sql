-- +goose Up
-- The GitHub poll tells a review TF submitted from one a person submitted by
-- whether a review artifact carries the review's numeric id as its external_id
-- (ArtifactStore.PresentExternalIDsSystem). It reads once per pull request
-- with new reviews; without this index each read walks every artifact the org
-- holds on the host.
CREATE INDEX idx_artifacts_external_id ON artifacts (org_id, scope, kind, external_id) WHERE external_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
