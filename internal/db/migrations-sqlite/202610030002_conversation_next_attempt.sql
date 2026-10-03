-- +goose Up
-- The earliest database time the conversation may next be claimed, set by a
-- hand-back that has to wait (db.HandBackPolicies names which outcomes do).
-- NULL = claimable whenever it otherwise matches. Cleared by the next claim, by
-- a person's message, by the stop settlement and by a resume. Stamped in the
-- claims lease layout (sqliteNowPlusExpr), the one the eligibility comparison
-- reads it against.
ALTER TABLE conversations ADD COLUMN next_attempt_at DATETIME;
-- +goose Down
SELECT 'down not supported';
