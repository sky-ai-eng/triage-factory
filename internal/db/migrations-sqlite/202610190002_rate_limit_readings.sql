-- +goose Up
-- The latest reading of every rate-limit bucket the org's credentials are
-- metered on, keyed (upstream, credential, bucket) within the org. The process
-- making requests keeps its readings in memory and writes the ones that changed
-- here, and GET /api/orgs/{org_id}/integrations/rate-limits reads them; an
-- upsert keeps a key's newest observation. credential is the opaque id the
-- upstream's client names it by, never a token or a login. A NULL column is a
-- value the reading did not state, which is not the same as zero; reset_at
-- NULL means the reading named no reset, and such a reading is reported for an
-- hour after observed_at (domain.RateLimitReadingMaxAge).
CREATE TABLE rate_limit_readings (
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    upstream     TEXT NOT NULL,
    credential   TEXT NOT NULL,
    bucket       TEXT NOT NULL,
    remaining    INTEGER,
    bucket_limit INTEGER,
    used         INTEGER,
    near_limit   BOOLEAN,
    reset_at     DATETIME,
    observed_at  DATETIME NOT NULL,
    PRIMARY KEY (org_id, upstream, credential, bucket)
);

-- +goose Down
SELECT 'down not supported';
