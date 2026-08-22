-- +goose Up
-- model_availability: per (org, provider, model) evidence that the org's
-- credentials can actually invoke that model, established by spending one
-- minimal real request on it.
--
-- A probe rather than a control-plane lookup, because no control-plane API
-- answers the question uniformly. Anthropic's /v1/models reflects the key's
-- access; Bedrock's ListFoundationModels reports what the REGION carries, not
-- what the account was granted, and returns base model ids rather than the
-- inference-profile ids that are the only invocable spelling for the current
-- Claude models. A probe needs no permission the run does not already need —
-- if the probe fails, the run would have failed too — and it tests the exact
-- id that will be sent on the wire.
--
-- Two stored states and a meaningful absence:
--
--   green   a probe succeeded. Terminal for automatic transitions: only a
--           manual re-test writes the row again, and a re-test that is refused
--           moves it to red.
--   red     the provider ANSWERED and refused (401/403, AccessDeniedException,
--           model-not-found). Cleared only by a later successful test.
--   absent  never probed, or every attempt so far was inconclusive — a 5xx, a
--           timeout, a dial failure. Those write NOTHING, so one bad provider
--           minute can never hide a model that works.
--
-- There is no TTL column and nothing sweeps this table. A green row is a fact
-- about a request that really succeeded, and re-checking would spend money to
-- learn something a dispatch failure already reports loudly. Every transition
-- is caused by a user gesture — connecting a credential, or pressing "test
-- connection" — which is also why there is no writer without request claims.
--
-- provider is part of the primary key rather than derived from model_key: what
-- the row establishes is that a CREDENTIAL PATH works, the eager sweep
-- addresses rows by provider, and a row must still say which credential it was
-- tested against after the build stops offering the key.
--
-- model_key carries no foreign key, for the same reason org_event_sources.kind
-- carries none: the catalog is a compiled-in file, not a table, so a row for a
-- key this build no longer offers is inert (nothing iterates to it) rather
-- than invalid.
--
-- Both modes write here. Local spends its probe through the agent runtime,
-- which reports the provider's HTTP status on its terminal event, so a
-- deployment authenticating through a Claude Code subscription — with no key
-- this process could assemble a request from — still gets a real answer.
CREATE TABLE model_availability (
    org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    model_key  TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('green', 'red')),
    checked_at DATETIME NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (org_id, provider, model_key)
);

-- +goose Down
SELECT 'down not supported';
