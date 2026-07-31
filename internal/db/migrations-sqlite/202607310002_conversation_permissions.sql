-- +goose Up
-- conversation_permissions: the durable record of every tool-approval prompt
-- a conversation raised, and how it was answered.
--
-- Before this table a pending approval existed in exactly two places — a map
-- on one process and one fire-once websocket frame — so a page refresh, a
-- second tab, or a cold board load could never learn that a healthy, parked
-- agent was waiting on a human. That is a reachability failure, not a
-- durability one: the state was valid and live, it simply had no address.
-- This gives it one, and the socket frame demotes to a hint that something
-- changed (the standing rule everywhere else in the app).
--
-- It is also the only place an approval is recorded at all. An allow used to
-- leave no trace whatsoever — the tool just ran — and a deny survived only as
-- prose inside a tool_result body, where "a human denied this" and "nobody was
-- watching" are indistinguishable without string-matching agent-facing English
-- that is deliberately free to change. reason as an enum column is what fixes
-- that; it is the point of the exercise.
--
-- The in-memory broker is still the mechanism that unblocks the agent (the
-- parked goroutine, the 1-buffered channel, the timers). This table is a
-- record kept alongside it, never the transport.
CREATE TABLE conversation_permissions (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    -- The engagement that asked. Pending is derived against this (see the
    -- partial index below): a row whose claim is no longer the conversation's
    -- active claim is a question asked by a process that no longer exists,
    -- about a workspace that may since have been restored, so it is not
    -- pending however the row itself reads.
    claim_id        TEXT REFERENCES claims(id) ON DELETE SET NULL,
    -- The assistant row carrying the gated tool_use block. Stays NULL on the
    -- SDK path: the wrapper's canUseTool fires from inside the SDK's tool
    -- dispatch, and the Go reader intercepts that control line on the same
    -- goroutine that drives the sink — so the prompt is raised before the
    -- assistant row it belongs to has been streamed, let alone inserted.
    message_id      INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    -- The SDK's toolUseID. The same id as messages.tool_calls[].id and the
    -- answering row's tool_call_id, so a decision ties back to the exact call
    -- it authorized.
    tool_call_id    TEXT NOT NULL,
    tool_name       TEXT NOT NULL,
    input_json      TEXT,
    -- The SDK's pre-rendered prompt sentence ("Claude wants to read foo.txt"),
    -- absent when it rendered none — a consumer must still be able to
    -- reconstruct a headline from tool_name + input.
    title           TEXT,
    -- App-validated: 'pending' | 'allowed' | 'denied' | 'expired'.
    state           TEXT NOT NULL,
    -- App-validated: 'user' | 'timeout' | 'absent' | 'closing' | 'claim_lost'.
    reason          TEXT,
    requested_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at      DATETIME,
    decided_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
    decided_at      DATETIME,
    -- How long the prompt stood open: milliseconds from requested_at to
    -- decided_at, stamped once at resolution. NULL while pending.
    --
    -- Stored rather than derived for the reason messages' own per-row
    -- duration is: subtracting the two timestamps at read time is a
    -- cross-clock subtraction (requested_at can come from the DB's
    -- CURRENT_TIMESTAMP default, decided_at from the app), so a consumer
    -- doing the arithmetic gets an answer that is wrong by whatever the two
    -- clocks disagree by. The write anchors it to the row's own stored
    -- requested_at, so the number is internally consistent whatever answered
    -- and wherever it ran.
    --
    -- Meaningful on every terminal state, not just an approval: "a human
    -- allowed it after 4s" and "it sat unattended for the full window" are
    -- the same measurement, and reading them together is how the timeout
    -- and absent-grace windows get tuned against what humans actually do.
    waited_ms       INTEGER
);

-- One row per gated call, which is also what makes the writer's insert safe to
-- reach twice.
CREATE UNIQUE INDEX idx_conv_perms_call ON conversation_permissions(conversation_id, tool_call_id);
-- The read this table exists for: "what is this conversation waiting on".
CREATE INDEX idx_conv_perms_pending ON conversation_permissions(conversation_id) WHERE state = 'pending';

-- No ON DELETE CASCADE from conversations, deliberately: an audit record whose
-- whole value is outliving the work must not be destroyed by cleaning up the
-- work. Conversations are archived (archived_at), not hard-deleted, in normal
-- operation, so the practical cost is that a hard delete of a conversation
-- carrying permission history is refused — the correct trade for this table.

-- +goose Down
SELECT 'down not supported';
