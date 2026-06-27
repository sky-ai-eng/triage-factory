-- +goose Up
-- external_actions (TFAC-483): the append-only audit log of record — one row per
-- external write Triage Factory performs under an ORG-scoped credential (the org
-- GitHub App / the org Jira service account). Both autonomous (bot/system) and
-- human-authorized-but-org-executed (the GitHub approval flow, the board-drag
-- draft toggle) writes land here; writes performed under an individual user's
-- OWN credential (the Jira claim/swipe/done/requeue flows) are deliberately
-- EXCLUDED and never instrumented. Event-grain and immutable — distinct from the
-- mutable, object-grain `artifacts` table (which stays the agent-run state aid).
--
-- Written in the SAME transaction as the action it records wherever the action
-- has a DB state change to compose with (the server approval flips), so the log
-- can't diverge from the action; the bot funnels record alongside the artifact
-- upsert under the same pool routing. action is a free-text discriminator
-- (domain.Action* — extensible, no CHECK). credential names the org credential
-- used (github_app | jira_org). from_state/to_state carry a transition's
-- endpoints. run_id is the producing run (the agent's, or the drafter's for an
-- approval); actor_user_id is the human authorizer/initiator (NULL for an
-- autonomous system write). detail_json carries the per-action payload.
--
-- dedup_key is the natural per-action key. A branch push is the one true
-- double-capture case — the git pre-push hook AND the git-proxy backstop both
-- observe the same push — so it carries a deterministic "branch:<run>:<ref>:<sha>"
-- key and the twin collapses under ON CONFLICT(org_id, dedup_key) DO NOTHING.
-- Every other action gets a unique key (the store fills a uuid when the caller
-- leaves it empty), so DO NOTHING only ever collapses the branch twin and never
-- silently drops a repeated action. Append-only: never UPDATE/DELETE.
--
-- SQLite is N=1 (local mode) so there is no RLS; the org_id default mirrors the
-- Postgres baseline. Mirrors the baseline's public.external_actions (TEXT ids;
-- DATETIME occurred_at). See TFAC-483.
CREATE TABLE external_actions (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id        TEXT,
    provider       TEXT NOT NULL,
    action         TEXT NOT NULL,
    target         TEXT NOT NULL,
    external_id    TEXT,
    url            TEXT,
    from_state     TEXT,
    to_state       TEXT,
    run_id         TEXT REFERENCES runs(id) ON DELETE SET NULL,
    actor_user_id  TEXT,
    credential     TEXT NOT NULL,
    dedup_key      TEXT NOT NULL,
    detail_json    TEXT,
    occurred_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_external_actions_dedup        ON external_actions (org_id, dedup_key);
CREATE INDEX        idx_external_actions_org_occurred  ON external_actions (org_id, occurred_at DESC);
CREATE INDEX        idx_external_actions_team_occurred ON external_actions (org_id, team_id, occurred_at DESC);
CREATE INDEX        idx_external_actions_run           ON external_actions (run_id);

-- +goose Down
SELECT 'down not supported';
