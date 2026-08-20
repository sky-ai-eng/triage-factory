package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// AuthEventStore owns the auth_events table — the SOC2 authentication audit log
// of record: durable, append-only capture of every authentication / session
// outcome (login, logout, refresh failure, JWT-verify failure, SSO enforcement,
// break-glass). The authentication sibling of AccessChangeLogStore (the
// authorization-CHANGE log). Capture only; the review/export UI is a deferred
// sibling. See TFAC-76.
//
// A SINGLE admin-pool write path — unlike AccessChangeLogStore's app-pool Record
// and ExternalActionStore's dual path: auth events NEVER carry user claims (the
// writes happen at the login critical edges, before/after a claims context
// exists, and org_id is frequently NULL), so an org-scoped RLS policy can't gate
// the table. In Postgres it is a system-only table (RLS deny-by-default, REVOKEd
// from the app roles) and every method runs on the admin (BYPASSRLS) pool —
// the same shape as SystemLLMRunStore. SQLite is N=1 and unscoped; local mode
// has no GoTrue so it produces ~no rows (parity-only table).
type AuthEventStore interface {
	// RecordSystem inserts one event on the ADMIN pool. Best-effort by contract:
	// a returned error is logged by the caller (the recordAuthEvent helper) and
	// the auth action proceeds regardless — NEVER rolled back. Append-only.
	// entry.ID may be empty (the impls generate a uuid; Postgres via
	// gen_random_uuid()); created_at is stamped server-side (the column DEFAULT).
	// Empty optional columns serialize to SQL NULL.
	//
	// Exempt from the returned-row rule: it appends to an audit log. The row
	// is written to be read by a later listing, never by its own writer.
	RecordSystem(ctx context.Context, entry domain.AuthEvent) error

	// ListByOrgSystem returns orgID's events newest-first (created_at DESC, id
	// DESC), bounded + filtered by opts. Admin pool. NULL-org rows are EXCLUDED
	// (they belong to no org; the future viewer surfaces them as
	// system/admin-only). For tests + the deferred viewer (no HTTP surface yet).
	ListByOrgSystem(ctx context.Context, orgID string, opts domain.AuthEventListOpts) ([]domain.AuthEvent, error)

	// ListByUserSystem returns userID's events newest-first, bounded + filtered
	// by opts. Admin pool. The personal "your logins" read the deferred viewer
	// will scope on.
	ListByUserSystem(ctx context.Context, userID string, opts domain.AuthEventListOpts) ([]domain.AuthEvent, error)

	// ClaimSeatSystem atomically claims a per-seat license slot for userID in the
	// given billing period (an opaque bucket key like "2026-07"), capping the
	// number of DISTINCT claimants at maxSeats. It owns the seat_claims ledger
	// (a login-critical-edge, admin-pool, deployment-scoped system table — the
	// same posture as auth_events, so it rides this store rather than a parallel
	// one). Returns:
	//   - admitted=true if userID already holds a claim for the period (idempotent
	//     returning user, count unchanged) OR a new claim was inserted within the
	//     cap; seats is the resulting claimant count.
	//   - admitted=false if granting a new claim would exceed maxSeats (the caller
	//     hard-blocks the login); seats is the count observed (== maxSeats).
	//
	// The count AND the claim run inside ONE transaction serialized per period
	// (Postgres advisory xact lock; SQLite's single-writer lock), so concurrent
	// first-logins cannot both read an under-cap count and both be admitted — the
	// TOCTOU a separate count-then-insert would leave open. Counts AUTHENTICATED
	// users: only the OAuth login path claims, so service/bot identities that
	// never traverse it are excluded for free. Admin pool, DEPLOYMENT-WIDE (no org
	// filter — a human is one seat regardless of how many orgs they belong to).
	ClaimSeatSystem(ctx context.Context, period, userID string, maxSeats int) (admitted bool, seats int, err error)
}
