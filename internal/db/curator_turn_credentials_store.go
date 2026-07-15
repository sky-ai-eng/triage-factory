package db

import "context"

// CuratorTurnCredentialsStore owns the curator_turn_credentials table — the
// sealed per-turn credential bundle channel, the curator-turn
// analog of RunCredentialsStore. A homed curator turn is a curator_requests
// row, not a runs row, so the run-keyed handshake can't carry it; this is the
// column-for-column mirror keyed on the request id instead.
//
// The home executor stands the turn's credential sidecar up and publishes its
// per-turn pubkey (Curator.PublishTurnCredPubKey); the brain resolves the
// turn's LLM/GitHub/Jira credentials, seals them to that key, and writes
// exactly one row here (Put). The executor polls it (Get), hands the OPAQUE
// sealed bytes to the sidecar, and best-effort deletes the row on teardown
// (Delete). Admin-pool-only in Postgres, same posture as RunCredentialsStore:
// this table never serves a request handler and its payload is credential-
// bearing ciphertext, so there is no app-pool grant at all.
//
// Both dialects, for store-interface + conformance-test symmetry with Postgres
// (CLAUDE.md's standing rule) — local mode (forced role=all) never calls
// Put/Get: the bundle path is executor-role-only.
type CuratorTurnCredentialsStore interface {
	// Put seals sealed (already-encrypted by the caller via credseal) into
	// curator_turn_credentials for requestID, replacing any existing row
	// wholesale. executorID/bootEpoch identify the claiming executor this
	// bundle was sealed for — the boot_epoch travels in cleartext so a reader
	// can compare it against its own current epoch BEFORE calling credseal.Open.
	//
	// Guarded on boot_epoch (never regresses it), identical to
	// RunCredentialsStore.Put: if an existing row already carries a STRICTLY
	// NEWER boot_epoch than this write's, the write is a silent no-op rather
	// than an overwrite. <=, not <, so a same-epoch refresh (re-minted tokens
	// for the SAME still-live turn) still applies normally.
	Put(ctx context.Context, orgID, requestID, executorID string, bootEpoch int64, sealed []byte) error

	// Get returns the sealed bundle for requestID, or ok=false when none has
	// been provisioned yet. Callers MUST compare the returned bootEpoch against
	// their own current boot_epoch before attempting to unseal — a bundle
	// sealed for an earlier boot must never be handed to credseal.Open.
	Get(ctx context.Context, orgID, requestID string) (executorID string, bootEpoch int64, sealed []byte, ok bool, err error)

	// Delete removes the turn's bundle — called best-effort on terminal
	// disposition (the turn finished) so stale sealed material doesn't linger.
	// Returns ok=false when no row matched. Rows also cascade with the request.
	Delete(ctx context.Context, orgID, requestID string) (ok bool, err error)
}
