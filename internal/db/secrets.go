package db

import (
	"context"
	"errors"
)

// ErrSecretStoreUnavailable is returned by every SecretStore method on a
// process that was never handed the secret-decryption key (TFAC-614):
// TF_ROLE=executor in multi mode, which never loads TF_SECRET_ENCRYPTION_KEY
// at boot — all per-run credential material arrives pre-resolved via sealed
// claim_credentials bundles instead. A distinct, greppable sentinel rather
// than a generic auth/decrypt failure, so a consumer that was missed when
// converting to the bundle path fails loudly at the first call instead of
// silently misbehaving.
var ErrSecretStoreUnavailable = errors.New("secret store not available on this role (executor role never holds the secret-decryption key)")

//go:generate go run github.com/vektra/mockery/v2 --name=SecretStore --output=./mocks --case=underscore --with-expecter

// SecretStore is the secret bag — GitHub PATs, Jira tokens, any other
// long-lived credential the product needs to custody. It has
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
//     multi. orgID must equal runmode.LocalDefaultOrgID; anything else
//     is a caller bug and rejected with an error.
//
//   - Multi mode: secrets are AES-256-GCM-encrypted app-side (a key
//     from TF_SECRET_ENCRYPTION_KEY, via internal/aead) and stored as
//     opaque ciphertext in the RLS-gated public.org_secrets table
//     (TFAC-402). RLS enforces the org gate (and, for per-user rows,
//     the user gate) on the app pool; the admin pool bypasses RLS for
//     the *System reads/writes. This replaced the original Supabase
//     Vault / pgsodium wrappers, whose root key lived in the postgres
//     container filesystem and was lost on any container recreate.
//
// D5 owns the consumer side (wiring real handlers + secret-name
// catalog); D2 provides the interface and both impls.
type SecretStore interface {
	// Put writes (or rotates) an org-scoped secret. description is
	// optional (stored as ""). The value is encrypted app-side and
	// stored in org_secrets keyed on (orgID, NULL user, key); rotations
	// overwrite the same row.
	//
	// Exempt from the returned-row rule. org_secrets does carry non-secret
	// row metadata (id, description, created_at, updated_at) — but no
	// existing read on this store ever answers with it: Get/GetSystem hand
	// back the decrypted value alone, never a projected column set, so
	// there is no point read whose column list and scanner a RETURNING
	// could share. The standard's mechanism is "project the point read's
	// shape onto the write" — this store has no row-shaped point read to
	// project, so applying it to Put means inventing a new read shape
	// nothing here has ever needed, which is a read-shape addition, not a
	// mechanical write-shape convergence (the ticket's own "if it converts
	// instead" case, needing its own domain type and matching point read
	// argued for on its own). Whatever a future row carried, it must still
	// exclude the ciphertext and the plaintext value themselves — widening
	// what a caller can obtain is the opposite of what this credential
	// surface exists to prevent. Same answer as ClaimCredentialsStore.Put,
	// for the same reason.
	Put(ctx context.Context, orgID, key, value, description string) error

	// Get returns the stored secret value, or ("", nil) when no
	// row matches (a missing secret is not an error — callers
	// distinguish "not configured" from "fetch failed" without
	// having to sniff sentinel errors).
	Get(ctx context.Context, orgID, key string) (string, error)

	// GetSystem reads an org-scoped secret WITHOUT a request JWT, for
	// background/system callers (webhook signature-verify reads, the
	// App-PEM backfill, multi-mode polling). Takes orgID explicitly:
	// in multi mode it runs on the admin pool (supabase_admin bypasses
	// RLS), which trusts the passed orgID and performs no
	// current_org_id() claims check; local mode forwards to the keychain
	// (single-org, no RLS, no claims). Mirrors Get's ("", nil)-on-absent
	// contract.
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
	// helpers on Stores (RequeueTask, SnoozeTask).
	Delete(ctx context.Context, orgID, key string) (ok bool, err error)

	// PutUser writes (or rotates) a per-user secret — a credential
	// custodied for one (orgID, userID) pair. The motivating case is
	// the Jira "act as yourself" credential (a Data Center PAT, or
	// later a Cloud OAuth refresh token); GitHub has no per-user
	// credential (identity-only), so this is genuinely net-new but
	// mirrors the org quartet exactly plus a userID dimension. Stored in
	// org_secrets keyed on (orgID, userID, key); rotations overwrite the
	// same row. description is optional (stored as "").
	//
	// # Key/host convention
	//
	// The store treats key opaquely. A user may hold credentials on
	// more than one Jira host, so host-scoping is the *consumer's* job:
	// it composes the key (e.g. "jira_token/<host>") before calling.
	// There is deliberately no separate host parameter here.
	//
	// Exempt from the returned-row rule, for the same reason as Put: GetUser
	// answers with a value, never a row, so there is no point read to
	// project org_secrets' non-secret columns onto — and nothing beyond
	// them may ever be handed back.
	PutUser(ctx context.Context, orgID, userID, key, value, description string) error

	// GetUser returns the stored per-user secret value, or ("", nil)
	// when no row matches — same "not configured" vs "fetch failed"
	// contract as Get. Claims-checked: in multi mode org_secrets RLS
	// filters out any row whose org_id != current_org_id() OR user_id !=
	// current_user_id(), so a handler running as user A reading user B's
	// token simply sees no row (("", nil), not a leak). That cross-user
	// gate is the load-bearing boundary this scope exists to provide.
	GetUser(ctx context.Context, orgID, userID, key string) (string, error)

	// GetUserSystem reads a per-user secret WITHOUT a request JWT, for
	// system code acting as a user — the write-actor resolver building
	// a jira.Resolver.ForUser client on a background run path is the motivating
	// caller. Takes orgID + userID explicitly: in multi mode it runs on
	// the admin pool (supabase_admin bypasses RLS), which trusts the
	// passed args and performs no current_org_id() / current_user_id()
	// claims check; local mode forwards to the keychain (single-user, no
	// RLS, no claims). Mirrors GetUser's ("", nil)-on-absent contract.
	//
	// System-code-only, same discipline as GetSystem. The app pool can't
	// reach another user's row (RLS filters it), so request handlers
	// stay on the claims-checked GetUser.
	GetUserSystem(ctx context.Context, orgID, userID, key string) (string, error)

	// PutUserSystem writes (or rotates) a per-user secret WITHOUT a request
	// JWT — the write-side mirror of GetUserSystem, for system code acting as
	// a user. The motivating caller is the Cloud OAuth access-token minter:
	// Atlassian rotates the refresh token on every refresh, so the minter must
	// persist the new refresh token back into the user's credential envelope,
	// and it runs on the system/background pool (the write-actor resolver holds
	// a claims-free store), so the claims-checked PutUser is unreachable.
	//
	// Takes orgID + userID explicitly: in multi mode it runs on the admin
	// pool (supabase_admin bypasses RLS), which trusts the passed args and
	// performs no current_org_id() / current_user_id() check; local mode
	// forwards to the keychain (single-user, no RLS, no claims).
	//
	// System-code-only, same discipline as GetUserSystem. The app pool's
	// RLS WITH CHECK would reject a write under another user's id, so
	// request handlers stay on the claims-checked PutUser.
	//
	// Exempt from the returned-row rule, same reason as Put/PutUser: no
	// point read on this store to project the row's non-secret columns
	// onto, and no outcome may echo back the ciphertext or plaintext
	// value.
	PutUserSystem(ctx context.Context, orgID, userID, key, value, description string) error

	// DeleteUser removes a per-user secret. Returns ok=false when no
	// row matched, mirroring Delete. Claims-checked like GetUser.
	DeleteUser(ctx context.Context, orgID, userID, key string) (ok bool, err error)
}
