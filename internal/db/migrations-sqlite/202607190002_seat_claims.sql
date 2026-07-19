-- +goose Up
-- seat_claims: the per-seat license ledger — one row per (billing period, user)
-- = one committed seat held for that period. The atomic seat-claim backing
-- per-seat enforcement (the login critical edge counts distinct claimants and
-- inserts a claim in one serialized write). PK (period, user_id) is the
-- idempotency + uniqueness key; count(*) for a period is the billable total.
--
-- Parity-only in SQLite: local mode is N=1 with no GoTrue login, so per-seat
-- enforcement never fires here. Kept shape-identical to the Postgres baseline
-- (public.seat_claims) so a future multi-user SQLite needs no rewiring.
CREATE TABLE seat_claims (
    period     TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (period, user_id)
);

-- +goose Down
SELECT 'down not supported';
