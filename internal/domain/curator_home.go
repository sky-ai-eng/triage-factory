package domain

import "time"

// CuratorHome is one row in curator_homes — the durable (org, project) -> home
// executor mapping that makes a curator session a placement-stable singleton
// (spec §6.3). Minted on the first turn from the capacity-weighted rendezvous
// winner over live executors, kept sticky while the home's heartbeat stays
// fresh, and re-minted by the next turn once the home goes heartbeat-stale
// (re-home on death).
//
// The row is placement coordination, not tenant content: like instances and
// placement_overrides it is admin-pool-only, org_id is a plain text column,
// and home_instance_id is not FK-validated (a since-retired home self-heals via
// the next turn's re-home, exactly like the rendezvous stamp).
type CuratorHome struct {
	OrgID     string
	ProjectID string

	// HomeInstanceID is the executor that owns this project's live session and
	// its warm cache. The home's claim loop scans for the project's queued
	// turns through this mapping; a control pod only rings the doorbell.
	HomeInstanceID string

	// HomeBootEpoch snapshots the home instance's registration epoch at mint
	// time, so a same-id/newer-boot home (a restarted executor) is legible in
	// the row rather than silently conflated with the prior boot.
	HomeBootEpoch int64

	HomedAt   time.Time
	UpdatedAt time.Time
}

// CuratorTurn is the minimal projection the executor claim loop reads off a
// claimable queued turn — a curator conversation with an undelivered plain
// user message and no active claim, whose project is homed to this executor.
// Enough to feed the per-project session goroutine (which re-reads full state
// under the requesting user's synthetic claims at dispatch). OrgID +
// CreatorUserID are the identity every per-turn write attributes to;
// ProjectID selects the session; MessageID is the turn id on the wire.
type CuratorTurn struct {
	MessageID      int64
	ConversationID string
	OrgID          string
	ProjectID      string
	CreatorUserID  string
}

// CuratorTurnProvision is the brain-side projection of a curator turn's
// credential-provisioning fields: what
// credprovision.ProvisionForCuratorTurn reads to resolve and seal the turn's
// bundle — the conversation's ACTIVE claim joined to the conversation. The
// claim carries the recipient key (CredPubKey, published by the home
// executor's sidecar bring-up) and the home the bundle is sealed for
// (HomeInstanceID = the claim's executor → its current boot_epoch); the
// conversation carries the project (GitHub authorized set = pinned ∩
// tracked) and the owning-team snapshot. No active claim = nothing to seal
// for. The curator-turn analog of the run path's AwaitingCredentialsConversation.
type CuratorTurnProvision struct {
	ConversationID string
	OrgID          string
	ProjectID      string
	TeamID         string
	HomeInstanceID string
	CredPubKey     string
}
