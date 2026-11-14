package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

	// Sources is the engagement's source manifest, read off its claims row
	// (claims.source_manifest), which Put writes in the same statement as the
	// ciphertext: which source credentials the bundle carries and why each
	// other one is absent. It rides this row's read because the executor reads
	// it beside the bundle and cannot answer the question itself — its secret
	// store is disabled and an event-triggered conversation has no user
	// identity to claim as — while the brain answers it with full knowledge on
	// the very pass that seals the bundle. Names, states and reasons only,
	// never secret material, so it travels in cleartext like
	// ExecutorID/BootEpoch/SealedAt above. Nil only when the claim carries no
	// manifest.
	Sources domain.SourceManifest
}

// ClaimCredentialsStore owns the claim_credentials table — the sealed
// per-claim credential bundle channel — and the source manifest on the claims
// row beside it. An executor parks a claimed conversation's active claim in
// phase='awaiting_credentials'; the brain resolves that conversation's
// LLM/GitHub/Jira/Linear credentials, seals them to the claimant's published
// pubkey, and writes exactly one row keyed by the id of the claim that
// published that pubkey, recording the source manifest on that claim in the
// same statement. Put names the claim, so a seal lands only on the claim whose
// key it used; Get names the conversation and reads its active claim's row.
// Admin-pool-only in Postgres,
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
type ClaimCredentialsStore interface {
	// Put writes sealed (already-encrypted by the caller via credseal) into
	// claim_credentials for claimID, replacing any existing row wholesale.
	// executorID/bootEpoch are the claim's identity as the caller read it
	// beside the key it sealed to. They travel in cleartext so a reader can
	// compare them against its own identity BEFORE calling credseal.Open (see
	// Get's doc).
	//
	// The write lands only while claimID is still active and still carries
	// executorID/bootEpoch. A seal for a claim released while the caller was
	// sealing lands nowhere, and never under the conversation's next claim,
	// whose sidecar published a different key and which the backstop sweep
	// stops sealing once it holds a bundle. A re-seal of the same claim
	// (re-minted tokens for a long-lived one) replaces the row.
	//
	// A nil error does NOT mean the bundle landed: with the claim released
	// between the caller's read and this write, Put writes nothing and returns
	// nil. Callers must treat delivery as confirmed only by the executor's own
	// Get poll, never by Put's return.
	//
	// Exempt from the returned-row rule, by decision rather than by shape:
	// the write is an upsert with a silent-no-op arm (the claim is no longer
	// active, or no longer carries that identity), so a RETURNING clause
	// reports zero rows on it — exactly the ON-CONFLICT-DO-NOTHING shape the
	// standard calls out as non-mechanical — and the store's own contract
	// above already tells callers not to read landedness off Put's return in
	// the first place. What a returned row could add beyond that is either
	// metadata the caller already supplied (claimID, executorID, bootEpoch)
	// or the sealed ciphertext itself, and handing the
	// latter back to satisfy a shape rule is worse than the rule: this
	// store's whole purpose is that sealed material flows into it and out
	// of it only through Get, gated on the reader's own boot_epoch. Same
	// answer as SecretStore's analogous write methods, for the same reason
	// — a credential surface with nothing non-secret worth echoing back.
	//
	// sources is the engagement's source manifest, written onto the claim's
	// row (claims.source_manifest) in the same statement, and only when the
	// ciphertext write itself applies, so the manifest always describes the
	// bundle the row holds. It stays on the claims row after the engagement
	// ends, which is what the fleet console reads for a finished one.
	Put(ctx context.Context, orgID, claimID, executorID string, bootEpoch int64, sealed []byte, sources domain.SourceManifest) error

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

// EncodeSourceManifest renders a source manifest for the claims.source_manifest
// column: nil → SQL NULL (no manifest), anything else → its JSON object. Both
// dialects write through it so the column holds one shape.
func EncodeSourceManifest(m domain.SourceManifest) (any, error) {
	if m == nil {
		return nil, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode source manifest: %w", err)
	}
	return string(data), nil
}

// DecodeSourceManifest parses the claims.source_manifest column as both
// dialects read it back: NULL → nil, anything else → the manifest it holds.
func DecodeSourceManifest(raw sql.NullString) (domain.SourceManifest, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var m domain.SourceManifest
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		return nil, fmt.Errorf("parse claims.source_manifest: %w", err)
	}
	return m, nil
}
