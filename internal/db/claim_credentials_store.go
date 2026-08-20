package db

import (
	"context"
	"time"
)

// SealedBundle is one row of the sealed per-claim credential channel: the
// opaque ciphertext plus the non-secret facts a reader needs BEFORE deciding
// whether to unseal it (and, for a reader that never can, instead of it).
type SealedBundle struct {
	// ExecutorID / BootEpoch identify the claiming executor this bundle was
	// sealed for. Both travel in cleartext specifically so a reader can
	// compare them against its own identity before calling credseal.Open —
	// see Get's doc.
	ExecutorID string
	BootEpoch  int64

	// Sealed is the ciphertext. Only the holder of the matching private key
	// (the claim's per-run credential sidecar) can open it.
	Sealed []byte

	// SealedAt is claim_credentials.created_at. Put replaces it on every
	// re-seal, so it dates the bundle currently in the row rather than the
	// claim's first provision — which makes it a freshness proof an
	// orchestrator can use without holding any unseal authority: "this bundle
	// was sealed after event X" is decidable from a timestamp alone. The
	// relayed `workspace add` gate is the caller that needs exactly that (the
	// provisioner recomputes a conversation's authorized repo set from scratch on
	// every pass, so a bundle sealed after a conversation_worktrees row
	// covers that row's repo by construction). Zero in local mode, which has
	// no sealed bundles at all.
	SealedAt time.Time
}

// ClaimCredentialsStore owns the claim_credentials table — the sealed
// per-claim credential bundle channel. An executor parks a claimed
// conversation's active claim in phase='awaiting_credentials'; the brain
// resolves that conversation's LLM/GitHub/Jira credentials, seals them to the
// claimant's published instances pubkey, and writes exactly one row keyed by
// the conversation's ACTIVE claim id. Method signatures still take
// conversationID (the conversation id) — the impl resolves the active claim
// internally, so callers never handle claim ids. Admin-pool-only in Postgres,
// same posture as InstanceStore/ConversationSignalStore: this table never
// serves a request handler, and unlike ConversationPendingInputStore its
// payload is credential-bearing ciphertext, so there is no app-pool grant at
// all.
//
// One row per claim (a re-claim after a crash is a NEW claim with its own
// fresh seal), replaced wholesale on every write (Put is an upsert) — a
// refresh (re-minted git tokens for a long-lived claim) simply overwrites
// the prior bundle, never merges into it.
//
// Postgres-only in substance: local mode reads the live secret store
// directly, so the SQLite schema has no claim_credentials table and the
// SQLite impl refuses with ErrNotApplicableInLocal.
// TODO(TFAC-838): these single-row writes still return a bare error and have
// not been converged on the returned-row standard: Put. Each is a genuine
// single-row insert/update/upsert, not one of the exempt buckets — the ticket
// tracks the conversion.
type ClaimCredentialsStore interface {
	// Put seals sealed (already-encrypted by the caller via credseal) into
	// claim_credentials for conversationID's active claim, replacing any existing
	// row wholesale. A conversation with no active claim is a silent no-op —
	// there is no engagement to seal for. executorID/bootEpoch identify the
	// claiming executor this bundle was sealed for — the boot_epoch travels
	// in cleartext specifically so a reader can compare it against its own
	// current epoch BEFORE calling credseal.Open (see Get's doc).
	//
	// Guarded on boot_epoch (never regresses it): if an existing row
	// already carries a STRICTLY NEWER boot_epoch than this write's, the
	// write is a silent no-op rather than an overwrite. This closes the
	// window where a slow provision for an older claim (timed out and
	// reclaimed by a different executor/boot while still in flight) would
	// otherwise land after, and clobber, the fresher claim's bundle —
	// self-healing either way via Get's epoch check and the backstop
	// sweep, but the guard means it never happens at all. <=, not <, so a
	// same-epoch refresh (re-minted tokens for the SAME still-live claim)
	// still applies normally.
	//
	// A nil error does NOT mean the bundle landed: with no active claim on
	// the conversation (released between the caller's read and this write),
	// Put inserts zero rows and returns nil. Callers must treat delivery as
	// confirmed only by the executor's own Get poll, never by Put's return.
	Put(ctx context.Context, orgID, conversationID, executorID string, bootEpoch int64, sealed []byte) error

	// Get returns the sealed bundle for conversationID's active claim, or ok=false
	// when none has been provisioned yet (or the conversation has no active claim).
	// Callers MUST compare the returned BootEpoch against their own
	// current boot_epoch before attempting to unseal — a bundle sealed for
	// an earlier boot must never be handed to credseal.Open, even though a
	// mismatched key would just fail the box auth tag; the contract is
	// stronger than "fails safe" (never attempt it at all), so a resume
	// path after a restart always re-requests provisioning instead.
	Get(ctx context.Context, orgID, conversationID string) (SealedBundle, bool, error)
}
