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

	// GetByWorkspaceIDSystem looks up a workspace by its Slack team ID alone
	// (no org_id to scope by), on the admin pool. The pre-auth webhook
	// receiver's only trusted input is the payload's team_id — this is how
	// it resolves which org (and which signing secret) a delivery belongs
	// to, before it has any other context to filter on. Returns (nil, nil)
	// when no workspace is connected under that id — an unknown team_id is
	// not an error, it just means the caller ack-and-drops.
	GetByWorkspaceIDSystem(ctx context.Context, workspaceID string) (*Workspace, error)
}

// DeliveryStore owns slack_event_deliveries — the cross-transport ingest
// dedup table (see the migration's comment for the full rationale). Postgres
// admin-pool only: the pre-auth webhook receiver has no request claims, and
// the future Socket Mode client is similarly claims-free. The SQLite impl is
// a stub returning db.ErrNotApplicableInLocal, same posture as
// WorkspaceStore.
type DeliveryStore interface {
	// MarkDeliveredSystem records one (workspaceID, eventID) delivery via an
	// insert-on-conflict, and opportunistically prunes deliveries older than
	// 72 hours in the same call. fresh=true means this delivery was not
	// previously recorded — the caller should process it; fresh=false means
	// a duplicate (a Slack retry) — the caller drops it silently. The prune
	// is best-effort: a failure there does not affect fresh's correctness
	// and is never surfaced as an error.
	MarkDeliveredSystem(ctx context.Context, workspaceID, eventID string) (fresh bool, err error)
}
