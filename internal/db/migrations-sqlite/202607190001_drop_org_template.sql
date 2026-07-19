-- +goose Up
-- The org template — a per-org, admin-editable copy of shipped defaults that
-- only ever affected teams created later — is fully unused: new orgs/teams
-- seed directly from the shipped Go slices (ai.ShippedPrompts() /
-- ai.ShippedBlueprints() / db.ShippedEventHandlers). Drop the four tables in
-- FK-safe order. Any admin-edited templates in these tables are intentionally
-- discarded.
DROP TABLE org_template_handlers;
DROP TABLE org_template_blueprint_steps;
DROP TABLE org_template_blueprints;
DROP TABLE org_template_prompts;

-- +goose Down
SELECT 'down not supported';
