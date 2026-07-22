-- +goose Up
-- run_messages becomes a hydration-grade context store: these columns are the
-- canonical representation a native agent loop rebuilds an exact LLM context
-- from (Postgres twin: internal/db/migrations-postgres/202605130001_pg_baseline.sql).
-- Backfill-free by design — every new column is nullable or defaults to the
-- value that makes every historical row (and every row the still-shipping SDK
-- runtime writes) mean exactly what it always meant.
--
-- reasoning: an array of reasoning-detail objects mirroring bifrost's
-- ChatReasoningDetails shape ({index, type, text?, signature?, data?}) — the
-- provider's replay reference for a prior turn's chain-of-thought. NULL means
-- no reasoning on this message. TEXT here (SQLite has no jsonb); Postgres
-- twin is jsonb.
ALTER TABLE run_messages ADD COLUMN reasoning TEXT;

-- content_blocks: an array of neutral content-block objects (bifrost's
-- ChatContentBlock shape: text / image / file), for ANY role — a tool row
-- uses this for an image result the flat `content` text column can't carry.
-- NULL means content is fully represented by `content`.
ALTER TABLE run_messages ADD COLUMN content_blocks TEXT;

-- delivered=0 marks a durable pending input (a steer / follow-up message) not
-- yet folded into any context assembly; the native loop flips it at its
-- injection points. Every row inserted by today's SDK runtime is delivered
-- immediately, hence the true default.
ALTER TABLE run_messages ADD COLUMN delivered BOOLEAN NOT NULL DEFAULT 1;

-- window_state is app-validated (no CHECK — same house pattern as
-- runs.origin): 'active' (default) | 'elided' (deterministic stub, content +
-- is_error retained) | 'inactive' (superseded by compaction, never
-- assembled). Flips are policy-gated and sticky by construction; nothing
-- flips this column in production yet.
ALTER TABLE run_messages ADD COLUMN window_state TEXT NOT NULL DEFAULT 'active';

-- seq is the assembly-order override: the effective sort key is
-- COALESCE(seq, id). NULL for every normally-appended row; only a synthetic
-- insertion (a compaction result) sets a fractional value to land between two
-- existing rows without renumbering either of them.
ALTER TABLE run_messages ADD COLUMN seq REAL;

-- Assembly order index: a native loop's ListForAssembly walks
-- (run_id, COALESCE(seq, id)). idx_run_messages_run (run_id only) stays for
-- the plain id-order transcript reads (UI, spend sums) that never touch seq.
CREATE INDEX idx_run_messages_run_seq ON run_messages(run_id, COALESCE(seq, id));

-- runtime picks the executing engine: 'sdk' (the Claude Code SDK subprocess —
-- every run today) or 'native' (the executor-side agent loop hydrating
-- straight from run_messages). App-validated, no value CHECK (mirrors
-- origin/source). See the Postgres twin's fuller comment on the replay
-- contract this discriminates.
ALTER TABLE runs ADD COLUMN runtime TEXT NOT NULL DEFAULT 'sdk';

-- +goose Down
SELECT 'down not supported';
