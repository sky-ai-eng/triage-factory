-- +goose Up
-- Widen user_jira_identities.source to admit 'cloud_api_token' — the provenance
-- marker the Cloud per-user API-token bind records (the paste counterpart of the
-- Data Center 'pat'). The value set stays closed (identity provenance is
-- security-relevant); this only adds the one Cloud paste method. Cloud OAuth
-- ('connect_oauth') was already allowed by the baseline.
ALTER TABLE public.user_jira_identities DROP CONSTRAINT user_jira_identities_source_check;
ALTER TABLE public.user_jira_identities ADD CONSTRAINT user_jira_identities_source_check
    CHECK ((source = ANY (ARRAY['pat'::text, 'connect_oauth'::text, 'scim'::text, 'cloud_api_token'::text])));

-- +goose Down
SELECT 'down not supported';
