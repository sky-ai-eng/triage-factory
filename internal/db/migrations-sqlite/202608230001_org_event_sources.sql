-- +goose Up
-- org_event_sources: declared per-(org, source) policy — today, whether an org
-- admin has turned a source off: no polling, no events, no tasks, and no
-- credential resolved for an agent, with the credential itself left bound.
-- Whether a given source may be turned off at all is a vocabulary question the
-- application answers (internal/eventsource.Disableable), not a constraint
-- here: kind is FK-free on purpose, and a row naming a source that has no off
-- switch is read as inert rather than obeyed.
--
-- Its own table rather than a column on org_settings, because the source
-- vocabulary is a RUNTIME REGISTRY (internal/eventsource.Register lets a source
-- outside core declare itself): a column per source would make every new source
-- a DDL change in both dialects, and would leave a source core holds no symbol
-- for with nowhere to record the fact. It is also not a column on
-- poll_readiness, whose rows are observed runtime state pruned when an org
-- stops polling — a prune of bookkeeping that silently re-enabled a source is a
-- bad failure mode to design in.
--
-- `disabled` is an explicit column rather than presence-is-off. A bare
-- disabled-sources table would be tighter for this one fact, but the row is
-- expected to grow (the per-(org, source) integration settings currently spread
-- across org_settings land here later), and the moment a row exists for a
-- configuration reason "row present" stops meaning "off". With more than one
-- fact on the row, an ABSENT row reads honestly as "no per-source overrides
-- recorded", which is exactly what every existing deployment has.
--
-- kind carries no foreign key on purpose: the vocabulary is a registry, not a
-- table, so a row naming a source this build does not carry is inert (nothing
-- resolves it) rather than invalid.
--
-- disabled_by names the admin who last flipped it and carries no foreign key
-- either: the audit trail of who did what lives in access_changes, and a
-- departed admin's deletion must not quietly rewrite the org's policy.
CREATE TABLE org_event_sources (
    org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    disabled    BOOLEAN NOT NULL DEFAULT 0,
    disabled_at DATETIME,
    disabled_by TEXT,
    PRIMARY KEY (org_id, kind)
);

-- +goose Down
SELECT 'down not supported';
