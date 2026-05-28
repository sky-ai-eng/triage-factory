package db

import (
	"context"
)

//go:generate go run github.com/vektra/mockery/v2 --name=SecretStore --output=./mocks --case=underscore --with-expecter

// SecretStore is the per-org secret bag — GitHub PATs, Jira tokens,
// any other long-lived credential the hosted product needs scoped to
// one tenant. Two production-grade implementations:
//
//   - Local mode: secrets live in the OS keychain (internal/auth),
//     keyed on the install — there's only one user. The SQLite impl
//     delegates Put/Get/Delete to auth.PutSecret/GetSecret/DeleteSecret
//     so callers that want one credential interface across modes
//     (local-equals-multi-at-N=1) get the same shape they'd get in
//     multi. orgID must equal runmode.LocalDefaultOrg; anything else
//     is a caller bug and rejected with an error.
//
//   - Multi mode: secrets are persisted via the public.vault_*
//     wrapper functions defined in the D3 baseline. Those functions
//     wrap Supabase Vault, enforce a creator_user_id-bound naming
//     convention ("org/<uuid>/<key>"), and refuse calls whose
//     request.jwt.claims.org_id doesn't match the p_org_id argument.
//     The Postgres impl is a thin wrapper over those SQL functions.
//
// D5 owns the consumer side (wiring real handlers + secret-name
// catalog); D2 provides the interface and both impls.
type SecretStore interface {
	// Put writes (or rotates) a secret. description is optional —
	// the wrapper coalesces NULL → "". Vault stores by name
	// "org/<orgID>/<key>"; rotations overwrite the same row.
	Put(ctx context.Context, orgID, key, value, description string) error

	// Get returns the stored secret value, or ("", nil) when no
	// row matches (a missing secret is not an error — callers
	// distinguish "not configured" from "fetch failed" without
	// having to sniff sentinel errors).
	Get(ctx context.Context, orgID, key string) (string, error)

	// GetSystem reads an org-scoped secret WITHOUT a request JWT, for
	// background/system callers (webhook signature-verify reads, the
	// App-PEM backfill, multi-mode polling). Takes orgID explicitly:
	// in multi mode it runs on the system/admin pool via
	// vault_get_org_secret_system, which trusts the passed orgID and
	// performs no current_org_id() claims check; local mode forwards
	// to the keychain (single-org, no RLS, no claims). Mirrors Get's
	// ("", nil)-on-absent contract.
	//
	// System-code-only. Request handlers must use the claims-checked
	// Get — same discipline as GetForOrgSystem vs GetForOrg. The one
	// sanctioned unauthenticated-orgID caller is the webhook handler:
	// its orgID comes from the URL path, but the secret is used only
	// to verify an incoming signature server-side and never leaves the
	// process, so a forged delivery for another org simply fails
	// verification.
	GetSystem(ctx context.Context, orgID, key string) (string, error)

	// Delete removes a secret. Returns ok=false when no row
	// matched, matching the pattern of other "did the write land"
	// helpers on Stores (RequeueTask, MarkAgentRunCancelledIfActive).
	Delete(ctx context.Context, orgID, key string) (ok bool, err error)
}
