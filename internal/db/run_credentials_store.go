package db

import "context"

// RunCredentialsStore owns the run_credentials table — the sealed per-run
// credential bundle channel (TFAC-614). An executor parks a claimed run in
// status='awaiting_credentials'; the brain resolves that run's LLM/GitHub/
// Jira credentials, seals them to the claimant's published instances
// pubkey, and writes exactly one row here. Admin-pool-only in Postgres,
// same posture as InstanceStore/RunSignalStore: this table never serves a
// request handler, and unlike RunPendingInputStore its payload is
// credential-bearing ciphertext, so there is no app-pool grant at all.
//
// One row per run, replaced wholesale on every write (Put is an upsert) —
// a refresh (re-minted git tokens for a long-running run) or a re-claim
// after a crash simply overwrites the prior bundle, never merges into it.
//
// Both dialects, for store-interface + conformance-test symmetry with
// Postgres (CLAUDE.md's standing rule) — local mode (forced role=all)
// never calls Put/Get: the bundle path is executor-role-only.
type RunCredentialsStore interface {
	// Put seals sealed (already-encrypted by the caller via credseal) into
	// run_credentials for runID, replacing any existing row wholesale.
	// executorID/bootEpoch identify the claiming executor this bundle was
	// sealed for — the boot_epoch travels in cleartext specifically so a
	// reader can compare it against its own current epoch BEFORE calling
	// credseal.Open (see Get's doc).
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
	Put(ctx context.Context, orgID, runID, executorID string, bootEpoch int64, sealed []byte) error

	// Get returns the sealed bundle for runID, or ok=false when none has
	// been provisioned yet. Callers MUST compare the returned bootEpoch
	// against their own current boot_epoch before attempting to unseal —
	// a bundle sealed for an earlier boot must never be handed to
	// credseal.Open, even though a mismatched key would just fail the box
	// auth tag; the contract is stronger than "fails safe" (never attempt
	// it at all), so a resume path after a restart always re-requests
	// provisioning instead.
	Get(ctx context.Context, orgID, runID string) (executorID string, bootEpoch int64, sealed []byte, ok bool, err error)

	// Delete removes the run's bundle — called on terminal disposition
	// (the run finished, was requeued, or was reaped) so stale sealed
	// material doesn't linger. Returns ok=false when no row matched.
	Delete(ctx context.Context, orgID, runID string) (ok bool, err error)
}
