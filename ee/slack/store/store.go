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
// Licensed under the Enterprise Edition License (see ee/LICENSE), not the
// repository-root BSL.
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

// Workspace is one row of org_slack_workspaces — a connected Slack
// workspace bound to exactly one org (workspace_id, Slack's own team ID, is
// the global PRIMARY KEY that enforces the bind — see the baseline
// migration). EnterpriseID is "" for a standalone workspace (NULL column);
// SigningSecretRef / AppTokenRef are "" when that credential was never
// supplied (NULL column) — exactly one is expected to be set for Socket
// Mode, the other for Events API, per Transport, though the schema does not
// enforce that pairing. RegisteredByUserID is "" when the registering user
// was later removed (ON DELETE SET NULL).
type Workspace struct {
	WorkspaceID        string
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
// org-admin); admin pool for ListAllSystem, the org-wide system read the
// future socket connection manager uses to enumerate every configured
// workspace at boot — annotated org-wide-system-job like
// EntityStore.ListUnclassified, not a per-request read.
type WorkspaceStore interface {
	// Upsert writes ws as the full desired end-state for its
	// (WorkspaceID, OrgID) pair: an existing same-org row is updated in
	// place, a wholly new workspace_id is inserted. A workspace_id already
	// bound to a DIFFERENT org surfaces the table's PRIMARY KEY violation
	// verbatim (a real unique-violation error, not a silent no-op) — the
	// caller maps that via httpx.IsUniqueViolation to a generic 409 that
	// never names the owning org.
	Upsert(ctx context.Context, ws Workspace) error
	ListForOrg(ctx context.Context, orgID string) ([]Workspace, error)
	Get(ctx context.Context, orgID, workspaceID string) (*Workspace, error)
	Delete(ctx context.Context, orgID, workspaceID string) error
	// ListAllSystem returns every connected workspace across every org, on
	// the admin pool (bypasses RLS). System-code-only — see the type doc.
	ListAllSystem(ctx context.Context) ([]Workspace, error)
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
