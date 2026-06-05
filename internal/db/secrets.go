package db

import (
	"context"
)

//go:generate go run github.com/vektra/mockery/v2 --name=SecretStore --output=./mocks --case=underscore --with-expecter

// SecretStore is the secret bag — GitHub PATs, Jira tokens, any other
// long-lived credential the hosted product needs to custody. It has
// two scopes: per-org (Put/Get/GetSystem/Delete) for tenant-wide
// credentials, and per-user (PutUser/GetUser/GetUserSystem/DeleteUser)
// for credentials bound to one (org, user) pair — the Jira "act as
// yourself" token being the motivating case. The two scopes share
// these implementations:
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

	// PutUser writes (or rotates) a per-user secret — a credential
	// custodied for one (orgID, userID) pair. The motivating case is
	// the Jira "act as yourself" credential (a Data Center PAT, or
	// later a Cloud OAuth refresh token); GitHub has no per-user
	// credential (identity-only), so this is genuinely net-new but
	// mirrors the org quartet exactly plus a userID dimension. Vault
	// stores by name "org/<orgID>/user/<userID>/<key>"; rotations
	// overwrite the same row. description is optional (NULL → "").
	//
	// # Key/host convention
	//
	// The store treats key opaquely. A user may hold credentials on
	// more than one Jira host, so host-scoping is the *consumer's* job:
	// it composes the key (e.g. "jira_token/<host>") before calling.
	// There is deliberately no separate host parameter here.
	PutUser(ctx context.Context, orgID, userID, key, value, description string) error

	// GetUser returns the stored per-user secret value, or ("", nil)
	// when no row matches — same "not configured" vs "fetch failed"
	// contract as Get. Claims-checked: in multi mode the vault wrapper
	// refuses when p_org_id != current_org_id() OR p_user_id !=
	// current_user_id(), so a handler running as user A cannot read
	// user B's token. That cross-user gate is the load-bearing
	// boundary this scope exists to provide.
	GetUser(ctx context.Context, orgID, userID, key string) (string, error)

	// GetUserSystem reads a per-user secret WITHOUT a request JWT, for
	// system code acting as a user — the write-actor resolver building
	// a JiraClientForUser on a background run path is the motivating
	// caller. Takes orgID + userID explicitly: in multi mode it runs on
	// the system/admin pool via vault_get_user_secret_system, which
	// trusts the passed args and performs no current_org_id() /
	// current_user_id() claims check; local mode forwards to the
	// keychain (single-user, no RLS, no claims). Mirrors GetUser's
	// ("", nil)-on-absent contract.
	//
	// System-code-only, same discipline as GetSystem. tf_app has no
	// EXECUTE on the underlying system function, so request handlers
	// cannot reach this door — they use the claims-checked GetUser.
	GetUserSystem(ctx context.Context, orgID, userID, key string) (string, error)

	// DeleteUser removes a per-user secret. Returns ok=false when no
	// row matched, mirroring Delete. Claims-checked like GetUser.
	DeleteUser(ctx context.Context, orgID, userID, key string) (ok bool, err error)
}
