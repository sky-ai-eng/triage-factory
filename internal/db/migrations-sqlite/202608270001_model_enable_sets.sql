-- +goose Up
-- Enable-sets replace the org's ranked model ceiling and the team's
-- provider restriction. Both of those were selection-time controls, and two
-- of them standing side by side could disagree; one multi-select per scope
-- cannot. A set also says which models may be picked without claiming any is
-- better than another, which is the claim a ceiling had to make and the
-- catalog declines to.
--
-- Only the ceiling is dropped here. The team restriction's column never
-- reached an install, so its own migration is gone rather than folded
-- forward: this epic's compatibility baseline is the last cut release, and a
-- column added and removed between two unreleased commits is not an upgrade
-- path anyone takes.

-- The model keys this org's teams may pick from, as a JSON array. NULL is
-- the absent value and means the org has expressed no preference, which
-- resolves to every model this deployment offers — so a model a later
-- release adds is enabled for that org the day it ships, while a stored set
-- stays frozen at what it names. The resolved set is never stored: "chose
-- nothing" and "chose everything" are different facts about what happens
-- next, and collapsing them at the column is what would make the first one
-- impossible to spell.
--
-- The keys are this dialect's own execution vocabulary — the harness aliases
-- a SQLite install dispatches, as its default_model and background_jobs_model
-- columns already are — because a set names models the deployment will be
-- asked to run.
--
-- JSON text rather than a child table: nothing semi-joins through
-- enablement, the model list is code so there is no FK target, and the set is
-- read whole at one choke point. App-validated against what this deployment
-- offers, not CHECK-constrained (the github_credential_class precedent) —
-- the accepted set changes with the binary, not with the schema.
ALTER TABLE org_settings ADD COLUMN enabled_models TEXT;

-- The org's ceiling over three Anthropic tier words. A rank over models is a
-- claim TF is not willing to assert, and a fourth tier arriving below the
-- floor (or a fifth between two existing ones) breaks any design that stored
-- ordinal position.
ALTER TABLE org_settings DROP COLUMN max_llm_model_tier;

-- Which of the org's enabled models this team may pick from, same encoding.
-- NULL inherits the org's effective set whole. A subset of that set at every
-- save, and narrowed to it again at every read, since the org may shrink its
-- own set afterwards and nothing rewrites a team row when it does.
ALTER TABLE team_settings ADD COLUMN enabled_models TEXT;

-- +goose Down
SELECT 'down not supported';
