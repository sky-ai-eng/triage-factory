-- +goose Up
-- system_llm_runs (TFAC-451): one row per agentproc.Run made by a headless
-- system job — the scorer, repo-profiler, and project-classifier each run a
-- Haiku call every poll cycle and today discard the cost the subprocess
-- already computed. Capturing per-call cost + token breakdown lets org spend
-- reconcile with the Anthropic bill and gives B1 a "system overhead" line
-- alongside runs.total_cost_usd and curator_requests.cost_usd.
--
-- Org-level, no team_id by design: scorer batches mix teams, and
-- repo_profiles/entities carry no team. SQLite is N=1 (local mode) so there
-- is no RLS; the org_id column + LocalDefaultOrgID default mirror the
-- Postgres baseline so a future multi-user SQLite shape needs no rewiring.
-- job is a free text discriminator ('scorer' | 'repo_profiler' | 'classifier',
-- extensible — no CHECK). Mirrors the Postgres baseline's system_llm_runs.
CREATE TABLE system_llm_runs (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    job                   TEXT NOT NULL,
    model                 TEXT NOT NULL,
    total_cost_usd        REAL NOT NULL DEFAULT 0,
    input_tokens          INTEGER NOT NULL DEFAULT 0,
    output_tokens         INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    duration_ms           INTEGER NOT NULL DEFAULT 0,
    num_turns             INTEGER NOT NULL DEFAULT 0,
    is_error              BOOLEAN NOT NULL DEFAULT 0,
    metadata_json         TEXT,
    started_at            DATETIME NOT NULL,
    completed_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_system_llm_runs_org_started     ON system_llm_runs(org_id, started_at DESC);
CREATE INDEX idx_system_llm_runs_org_job_started ON system_llm_runs(org_id, job, started_at DESC);

-- +goose Down
SELECT 'down not supported';
