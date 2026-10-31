// Package knowledgeevent owns the "team_knowledge_updated" websocket event —
// the invalidation ping the Knowledge page refreshes on after a team's KB is
// written.
//
// It is an invalidation ping, not a sparse update: the payload names WHICH
// listing went stale and nothing about what changed, and the client refetches
// through the REST read, which carries the visibility scoping. Pushing entries
// would mean hand-mirroring the two-root read policy onto a hub that has no
// policy enforcement point, for a page whose listing is one request away.
//
// The delivery scope is the root that changed, because the root IS the
// visibility model:
//
//   - shared/ is readable by any member of the org, so a change there is one
//     org-scoped envelope — the hub's own (org, user) filter is already exactly
//     that audience.
//   - private/ is readable only through its own team's gate, so a change there
//     is resolved to the team's members at emit time and fanned out one
//     envelope per user id on the hub's UserID axis. Resolution is a fresh read
//     per emission: nothing is captured on the connection, so a membership
//     change takes effect on the next event instead of going stale into the
//     disclosure this scoping exists to prevent.
package knowledgeevent

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

var knowledgeLog = logging.Component("knowledgeevent")

// EventType is the websocket event type for a team knowledge-base change.
const EventType = "team_knowledge_updated"

// Update is the ping's payload: which team's KB changed, and in which root.
// Both are identity — what to refetch — never a diff of what was written. The
// root is carried so a page showing one root does not refetch on a change to
// the other, which is the same reason the REST list takes a root filter.
type Update struct {
	TeamID string       `json:"team_id"`
	Root   kbstore.Root `json:"root"`
}

// MembersFunc resolves the user ids enrolled on teamID in orgID — the audience
// for a private-root change. db.TeamsStore.MemberIDsSystem is the production
// implementation.
type MembersFunc func(ctx context.Context, orgID, teamID string) ([]string, error)

// Broadcaster is the minimal hub surface Publish needs; *websocket.Hub
// satisfies it (and is nil-receiver-safe, so a typed-nil hub no-ops).
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// Notifier publishes team_knowledge_updated events. members == nil is the
// local-mode shape (N=1: one org, one user, no team boundary to mirror), where
// every change is one org-scoped envelope; a non-nil members is the multi-mode
// shape, where a private-root change fans out per member.
type Notifier struct {
	hub     Broadcaster
	members MembersFunc
}

// NewNotifier builds a Notifier over hub. Pass members only in multi mode. A
// nil hub yields a Notifier whose Publish no-ops, matching the hub's own
// nil-safety so callers don't guard emit sites.
func NewNotifier(hub Broadcaster, members MembersFunc) *Notifier {
	return &Notifier{hub: hub, members: members}
}

// Publish announces that teamID's KB changed under each of roots. A write that
// spans both roots — publishing a folder moves it out of one and into the
// other — passes both, and each is delivered to its own audience rather than
// to the wider of the two.
//
// A resolution failure drops that envelope with a log. Fail closed: a missed
// ping costs the viewer a manual refresh, while falling back to an org-wide
// broadcast would announce a private-root change to the whole org.
func (n *Notifier) Publish(ctx context.Context, orgID, teamID string, roots ...kbstore.Root) {
	if n == nil || n.hub == nil || teamID == "" {
		return
	}
	for _, root := range dedupeRoots(roots) {
		n.publishOne(ctx, orgID, teamID, root)
	}
}

func (n *Notifier) publishOne(ctx context.Context, orgID, teamID string, root kbstore.Root) {
	upd := Update{TeamID: teamID, Root: root}
	// A shared-root change is org-readable, so the hub's own org filter is the
	// audience: fanning it per user would be N envelopes for the same reach.
	if root == kbstore.RootShared || n.members == nil {
		n.hub.Broadcast(websocket.Event{Type: EventType, OrgID: orgID, Data: upd})
		return
	}
	userIDs, err := n.members(ctx, orgID, teamID)
	if err != nil {
		knowledgeLog.Error("resolve team members failed; dropping event", "team", teamID, "root", root, "error", err)
		return
	}
	for _, uid := range userIDs {
		// An empty id would silently widen this envelope to the whole org (the
		// hub reads an empty evt.UserID as "not user-specific"), so it is
		// dropped like every other resolution fault — and logged, because the
		// store contract (NOT NULL user_id) says it cannot happen.
		if uid == "" {
			knowledgeLog.Error("team members returned an empty user id; dropping that envelope", "team", teamID)
			continue
		}
		n.hub.Broadcast(websocket.Event{Type: EventType, OrgID: orgID, UserID: uid, Data: upd})
	}
}

// dedupeRoots keeps the caller's order and drops repeats and unknown values, so
// one write that touched a root twice sends one ping and a malformed root sends
// none.
func dedupeRoots(roots []kbstore.Root) []kbstore.Root {
	out := make([]kbstore.Root, 0, len(roots))
	seen := make(map[kbstore.Root]struct{}, len(roots))
	for _, r := range roots {
		if _, err := kbstore.ParseRoot(string(r)); err != nil {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}
