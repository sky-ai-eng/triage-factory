// Package slackstore is the Enterprise Edition Slack data layer: the domain
// types, the store interface, and the typed accessors that retrieve the
// Slack store bundle from core's opaque per-transaction extension slot.
//
// Keeping the Slack types and interface here is what lets core hold zero
// Slack symbols: core's db.TxStores / db.Stores carry only an opaque Ext
// map; this package supplies the typed view back. The backend
// implementations live in the pg/ and lite/ subpackages, which register
// their factories with core's store-extension registry (see store/pg,
// store/lite) — mirrors ee/sso/store exactly.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package slackstore

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ExtKey is the registry key the Slack store bundle is registered + read
// under in core's db.TxStores / db.Stores extension slot.
const ExtKey = "slack"

// Bundle is the Slack store set for one construction (one tx, or the
// non-tx aggregate). It is the value the registered factory returns as
// `any` and that FromTx / FromStores assert back to.
type Bundle struct {
	Workspaces WorkspaceStore
	Identities SlackIdentityStore
	// Deliveries owns slack_event_deliveries — the cross-transport ingest
	// dedup table. Admin-pool only (see DeliveryStore); read through
	// FromStores, never FromTx (the pre-auth webhook receiver has no tx).
	Deliveries DeliveryStore
}

// FromTx returns the tx-bound Slack bundle, or nil if no Slack extension is
// registered (a community build with the Enterprise Edition not linked
// in — in which case no Slack handler is mounted to call this).
func FromTx(tx db.TxStores) *Bundle {
	b, _ := tx.Extension(ExtKey).(*Bundle)
	return b
}

// FromStores returns the non-tx Slack bundle (admin-pool reads — the
// future socket connection manager's boot-time enumeration), or nil if no
// Slack extension is registered.
func FromStores(s db.Stores) *Bundle {
	b, _ := s.Extension(ExtKey).(*Bundle)
	return b
}

// Workspace is one row of org_slack_workspaces — a connected (Slack
// workspace, Slack app) pair bound to exactly one org. (WorkspaceID,
// APIAppID) — Slack's own team ID and app ID — is the global composite
// PRIMARY KEY that enforces the workspace+app bind (see the baseline
// migration); the stronger "this app belongs to exactly one org, across
// every workspace" invariant has no SQL constraint to lean on (APIAppID
// alone isn't unique) and is enforced by the connect handler instead (see
// WorkspaceStore.AppBoundToOtherOrgSystem). EnterpriseID is "" for a
// standalone workspace (NULL column); SigningSecretRef / AppTokenRef are ""
// when that credential was never supplied (NULL column) — exactly one is
// expected to be set for Socket Mode, the other for Events API, per
// Transport, though the schema does not enforce that pairing.
// RegisteredByUserID is "" when the registering user was later removed (ON
// DELETE SET NULL).
type Workspace struct {
	WorkspaceID        string
	APIAppID           string
	OrgID              string
	WorkspaceName      string
	EnterpriseID       string
	Transport          string // "socket" | "events_api"
	BotUserID          string
	BotTokenRef        string
	SigningSecretRef   string
	AppTokenRef        string
	RegisteredByUserID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// WorkspaceStore owns the org_slack_workspaces table. Postgres-only; the
// SQLite impl is a stub returning db.ErrNotApplicableInLocal (SSO posture:
// local mode is N=1, no multi-workspace concept). App pool for the
// org-admin-gated CRUD (RLS gates SELECT on org membership, writes on
// org-admin); admin pool for ListAllSystem/GetByWorkspaceAppSystem/
// AppBoundToOtherOrgSystem, the org-wide system reads a claims-free caller
// (the pre-auth webhook receiver, the future socket connection manager, the
// connect handler's own cross-org invariant check) needs — annotated
// org-wide-system-job like EntityStore.ListUnclassified, not a per-request
// read.
type WorkspaceStore interface {
	// Upsert writes ws as the full desired end-state for its
	// (WorkspaceID, APIAppID, OrgID) triple: an existing same-org row for
	// that (workspace, app) pair is updated in place, a wholly new
	// (workspace_id, api_app_id) pair is inserted. A (workspace_id,
	// api_app_id) pair already bound to a DIFFERENT org surfaces the
	// table's PRIMARY KEY violation verbatim (a real unique-violation
	// error, not a silent no-op) — the caller maps that via
	// httpx.IsUniqueViolation to a generic 409 that never names the owning
	// org. Callers MUST call LockApp and check AppBoundToOtherOrgSystem
	// before calling Upsert — the PK violation alone does not catch a
	// different workspace_id already carrying this org's api_app_id under
	// another org, since api_app_id isn't unique on its own.
	Upsert(ctx context.Context, ws Workspace) error
	ListForOrg(ctx context.Context, orgID string) ([]Workspace, error)
	Get(ctx context.Context, orgID, workspaceID, apiAppID string) (*Workspace, error)
	Delete(ctx context.Context, orgID, workspaceID, apiAppID string) error
	// ListAllSystem returns every connected workspace across every org, on
	// the admin pool (bypasses RLS). System-code-only — see the type doc.
	ListAllSystem(ctx context.Context) ([]Workspace, error)

	// GetByWorkspaceAppSystem looks up a workspace by its (Slack team ID,
	// Slack app ID) pair alone (no org_id to scope by), on the admin pool.
	// The pre-auth webhook receiver's only trusted input is the payload's
	// team_id + api_app_id — this is how it resolves which org (and which
	// signing secret) a delivery belongs to, before it has any other
	// context to filter on. Returns (nil, nil) when no workspace is
	// connected under that pair — an unknown (team_id, api_app_id) is not
	// an error, it just means the caller ack-and-drops.
	GetByWorkspaceAppSystem(ctx context.Context, workspaceID, apiAppID string) (*Workspace, error)

	// LockApp acquires a transaction-scoped Postgres advisory lock keyed on
	// apiAppID, on the app pool (it must run in the SAME transaction as the
	// AppBoundToOtherOrgSystem check and the Upsert that follow it, so its
	// lifetime is tied to that transaction's commit/rollback). The connect
	// handler calls this FIRST, before anything else in its transaction.
	//
	// Without it, "read AppBoundToOtherOrgSystem, then Upsert" is a
	// check-then-act race: two concurrent connects for the same api_app_id
	// under different orgs write different workspace_id rows, so they never
	// collide on a unique index, and each can observe "not bound" before
	// either commits. LockApp closes that gap — every connect for a given
	// api_app_id serializes on this call. A concurrent transaction blocks
	// here until the current holder commits or rolls back, so by the time
	// it proceeds, the invariant it's about to check and enforce is
	// already consistent. Releases automatically at transaction end; there
	// is no corresponding unlock call.
	LockApp(ctx context.Context, apiAppID string) error

	// AppBoundToOtherOrgSystem reports whether apiAppID is already bound to
	// a TF org other than orgID, on the admin pool (bypasses RLS — the
	// app-single-org invariant spans every org, not just the caller's, so
	// an RLS-scoped read that only sees the caller's own rows could never
	// answer this). The connect handler calls this INSIDE its transaction,
	// after LockApp and before Upsert, to enforce "a Slack app belongs to
	// exactly one TF org" — a rule with no SQL constraint behind it, since
	// api_app_id is only ever unique in combination with workspace_id.
	// Mapped to a generic 409 that never names the owning org.
	AppBoundToOtherOrgSystem(ctx context.Context, apiAppID, orgID string) (bool, error)
}

// DeliveryStore owns slack_event_deliveries — the cross-transport ingest
// dedup table (see the migration's comment for the full rationale). Keyed on
// api_app_id, not workspace_id: delivery streams are honestly per-app (a
// Socket Mode connection belongs to the app, and Slack load-balances an
// app's envelopes across all of that app's open sockets regardless of which
// workspace(s) it's installed in), so two apps sharing a workspace must not
// share a dedup key. Postgres admin-pool only: the pre-auth webhook receiver
// has no request claims, and the future Socket Mode client is similarly
// claims-free. The SQLite impl is a stub returning
// db.ErrNotApplicableInLocal, same posture as WorkspaceStore.
type DeliveryStore interface {
	// MarkDeliveredSystem records one (apiAppID, eventID) delivery via an
	// insert-on-conflict, and opportunistically prunes deliveries older than
	// 72 hours in the same call. fresh=true means this delivery was not
	// previously recorded — the caller should process it; fresh=false means
	// a duplicate (a Slack retry) — the caller drops it silently. The prune
	// is best-effort: a failure there does not affect fresh's correctness
	// and is never surfaced as an error.
	MarkDeliveredSystem(ctx context.Context, apiAppID, eventID string) (fresh bool, err error)
}

// SlackIdentity is one row of user_slack_identities — the mapping from a
// Slack (workspace, sender) pair to the TF user it was resolved to, if any
// (TFAC-531). UserID is nil for an attempted-but-unmatched sender: the
// negative cache that stops a repeat mention from re-triggering a
// users.info call inside the resolver's retry TTL. SlackDisplayName is the
// Slack-side name captured at resolution time, stored here because the
// auth-principal bridge this resolves against holds no display name of its
// own that's safe to render in audit UI.
type SlackIdentity struct {
	WorkspaceID      string
	SlackUserID      string
	UserID           *string // nil = attempted, unmatched (the negative cache)
	SlackDisplayName string
	Source           string // "auto_email" | "oidc" (oidc: future Sign-in-with-Slack leaf)
	LastAttemptAt    time.Time
}

// SlackIdentityStore owns the user_slack_identities table — inbound,
// system-initiated capture of the human behind a Slack mention (as opposed
// to WorkspaceStore's self-initiated org-admin connect flow). Every method
// is admin-pool (`...System`): capture runs in a detached goroutine after
// the mention's event publishes, where no request claims context exists —
// see the RLS notes on the table's migration. Postgres-only; the SQLite
// impl is a stub returning db.ErrNotApplicableInLocal (same posture as
// WorkspaceStore).
//
// No set-valued reverse-lookup method exists here (unlike
// UserIDsForGitHubLoginSystem / UserIDsForJiraAccountSystem): the table's
// PRIMARY KEY is (workspace_id, slack_user_id), which guarantees at most one
// mapping per Slack sender, so LookupSystem is the whole read surface.
type SlackIdentityStore interface {
	// LookupSystem returns the identity row for (workspaceID, slackUserID),
	// or (nil, nil) if no row exists yet (never attempted).
	LookupSystem(ctx context.Context, workspaceID, slackUserID string) (*SlackIdentity, error)

	// UpsertResolvedSystem writes a positive match: userID is bound to
	// (workspaceID, slackUserID) with source='auto_email' and
	// last_attempt_at reset to now. Overwrites any prior row for this key,
	// including a previously resolved DIFFERENT user (a re-resolve always
	// wins — there is no ambiguity to preserve once a single match is
	// found).
	//
	// NOT an org/team membership check: userID is resolved by
	// db.UsersStore.UserIDsForVerifiedEmailSystem, which matches globally
	// across the deployment (principals are deliberately cross-org
	// identities here, same as the login-time auto-link it mirrors — see
	// that method's doc). This mapping alone does not establish that userID
	// belongs to the org that owns workspaceID. Future consumers (run-
	// visibility grants, audit rendering — TFAC-510/511) MUST check org/team
	// membership at the point of granting access, the same way every other
	// visibility surface in this codebase does; they must never treat a row
	// here as membership proof on its own.
	UpsertResolvedSystem(ctx context.Context, workspaceID, slackUserID, userID, displayName string) error

	// MarkAttemptSystem upserts the negative-cache row: user_id NULL,
	// last_attempt_at = now(), for a sender that was looked up and
	// definitively did not resolve (zero or ambiguous verified-email
	// matches, or a bot/deleted/no-email sender). MUST NOT clobber an
	// existing RESOLVED row's user_id — a stale negative-cache write racing
	// behind a resolution must never un-resolve it. Implementations enforce
	// this with ON CONFLICT ... DO UPDATE ... WHERE user_id IS NULL, not a
	// pre-read-then-write (which would race).
	MarkAttemptSystem(ctx context.Context, workspaceID, slackUserID, displayName string) error
}
