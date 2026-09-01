package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubPendingBindTTL is how long an initiated bind ceremony stays
// completable. Minutes, not hours: the window a human needs is the time it
// takes to pick an account on GitHub's install page and press the button, and
// everything past that is a record an attacker could still race a victim into
// spending.
const GitHubPendingBindTTL = 15 * time.Minute

// GitHubPendingBindPruneAge is how far past its expiry a record is kept before
// the opportunistic prune sweeps it. The delay buys nothing operationally —
// nothing reads an expired row — and exists so a prune racing a legitimate
// consume cannot delete the row the consume is about to fail on for a
// different, less legible reason.
const GitHubPendingBindPruneAge = time.Hour

// GitHubPendingBindStore owns github_pending_binds — one row per initiated
// bind ceremony, the durable half of the CSRF pair that proves a returning
// installation belongs to the workspace that asked for it.
//
// The cookie carries the nonce and the row carries its hash, so neither half
// alone completes a bind: a database read yields no usable nonce, and a stolen
// cookie names no workspace. What the pair stops is PLANTING — an attacker
// completing an install themselves, then inducing a signed-in TF admin to load
// our callback with their code and installation id, which would put the
// attacker's repositories into the victim's workspace.
//
// Admin-pool-only in Postgres, and that is forced rather than chosen: the
// consume reads the row BY ITS NONCE HASH and learns the org from it, so the
// org an RLS policy would gate on is the read's OUTPUT, not an input. The
// nonce is the authorization, exactly as an invite token is on the redeem path.
// The create side is gated in the handler instead (org admin), which is the
// only place that check can live.
//
// Real in both dialects rather than a local stub: the ceremony itself is
// multi-only (local ships no shared App key), but the store is dialect-neutral
// and the conformance suite runs against both, so the atomic-consume guarantee
// is proven by the same assertions on either backend.
type GitHubPendingBindStore interface {
	// CreateSystem writes one pending-bind record and returns the row it
	// persisted, read off RETURNING on the insert itself. CreatedAt and
	// ExpiresAt are the caller's — the ceremony's clock is the handler's, and
	// a column default would leave the TTL spelled in two dialects instead of
	// one constant.
	//
	// System (claims-free) by construction: it writes on the admin pool for
	// the same reason the consume reads there, and the org-admin check that
	// authorizes it has already run in the handler.
	CreateSystem(ctx context.Context, bind domain.GitHubPendingBind) (domain.GitHubPendingBind, error)

	// ConsumeSystem spends the record whose nonce hashes to nonceHash and
	// returns it, or nil when there is nothing to spend. It also
	// opportunistically prunes records long past their expiry, the way
	// GitHubDeliveryStore.MarkDeliveredSystem prunes deliveries.
	//
	// Single-use is the whole contract, and it is a conditional UPDATE …
	// RETURNING rather than a read followed by a write: two callbacks arriving
	// on one record at the same instant must not both proceed, and a
	// read-then-write leaves exactly that window open.
	//
	// Absent, expired and already-consumed collapse into one nil answer on
	// purpose. The caller refuses identically for all three — the ticket's
	// "no cookie, no record, expired, or already consumed → refuse" — so
	// distinguishing them would only offer an unauthenticated caller a way to
	// probe which nonces once existed.
	ConsumeSystem(ctx context.Context, nonceHash string, now time.Time) (*domain.GitHubPendingBind, error)
}
