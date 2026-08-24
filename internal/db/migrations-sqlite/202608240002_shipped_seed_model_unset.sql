-- +goose Up
-- A shipped prompt seed names no model at all: it provisions unset and inherits
-- the team default at dispatch. A seed goes to every install whatever its
-- credentials, so a concrete pick in one is not a choice anybody made — it is a
-- default nobody was asked about, spending against a model they did not choose.
-- The rows those seeds already wrote are brought to the same state here.
--
-- A user's own pin is a choice, and is left exactly as it is. So is an
-- imported prompt's, which came from somewhere a person controls.
--
-- Only prompts. conversations.model and the per-message model stamps record
-- what actually ran, and a historical row re-labelled would make the transcript
-- and the ledger lie about a run that already happened.
--
-- Nothing here translates a vocabulary. This dialect is local mode, whose
-- conversations are executed by the Claude Code SDK subprocess, and the words
-- below are that harness's own family aliases — what a local install stores,
-- sends, and offers in its picker. The concrete wire ids belong to the native
-- runtime, which is the Postgres dialect's.
UPDATE prompts SET model = ''
 WHERE model <> '' AND source = 'system';

-- +goose Down
SELECT 'down not supported';
