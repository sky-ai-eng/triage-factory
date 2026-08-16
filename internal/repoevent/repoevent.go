// Package repoevent owns the "repository_updated" websocket event — the
// single sparse-diff notification for the org-wide repositories table,
// unifying the pair of per-column-group events that preceded it.
//
// Two properties are load-bearing:
//
//   - Update is an explicit allowlist. The repositories row carries
//     columns nobody renders (poll ETags, timestamps) and columns that
//     must never reach a client outside the repo's audience; only the
//     fields on Update can go over the wire, and adding one is a
//     deliberate schema change here, not a map key at an emit site.
//   - Delivery scope mirrors the REST read. The table is org-wide but
//     /api/repos is visibility-scoped (org admins see the union,
//     members only their teams' tracked sets), and the websocket hub
//     scopes connections on (org, user) only — it has no team axis. So
//     a Notifier built with a RecipientsFunc resolves the audience at
//     emit time and fans out one event per user id on the hub's UserID
//     axis. Resolution is a fresh read per emission, so a
//     team-membership change takes effect on the next event — nothing
//     is captured per connection, nothing goes stale.
package repoevent

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

var repoeventLog = logging.Component("repoevent")

// EventType is the websocket event type for repositories-row updates.
const EventType = "repository_updated"

// Update is the sparse-diff payload: ID plus ONLY the fields the emitter
// just wrote. Nil pointer fields are omitted from the wire JSON, and the
// frontend merges whichever fields are present into its row by id —
// so a clone-status event never blanks the AI profile text and vice
// versa. New values only: there is deliberately no before-image (see
// the websocket-events section of CLAUDE.md).
type Update struct {
	ID             string  `json:"id"` // "owner/repo"
	HasReadme      *bool   `json:"has_readme,omitempty"`
	HasClaudeMd    *bool   `json:"has_claude_md,omitempty"`
	HasAgentsMd    *bool   `json:"has_agents_md,omitempty"`
	ProfileText    *string `json:"profile_text,omitempty"`
	CloneStatus    *string `json:"clone_status,omitempty"`
	CloneError     *string `json:"clone_error,omitempty"`
	CloneErrorKind *string `json:"clone_error_kind,omitempty"`
}

// Ptr adapts a literal to Update's optional-field pointers at emit sites.
func Ptr[T any](v T) *T { return &v }

// RecipientsFunc resolves the user ids that may receive an Update for
// (owner, repo) in orgID — the WS mirror of the REST read's visibility.
// db.TeamGitHubReposStore.RepoUpdateRecipientsSystem is the production
// implementation.
type RecipientsFunc func(ctx context.Context, orgID, owner, repo string) ([]string, error)

// Broadcaster is the minimal hub surface Publish needs; *websocket.Hub
// satisfies it (and is nil-receiver-safe, so a typed-nil hub no-ops).
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// Notifier publishes repository_updated events with the delivery scope
// resolved per its construction: recipients == nil is the local-mode
// shape (one org-scoped broadcast — N=1, no team boundary), a non-nil
// recipients is the multi-mode shape (per-user fan-out).
type Notifier struct {
	hub        Broadcaster
	recipients RecipientsFunc
}

// NewNotifier builds a Notifier over hub. Pass recipients only in multi
// mode; nil keeps the single org-scoped broadcast local mode has always
// done. A nil hub yields a Notifier whose Publish no-ops, matching the
// hub's own nil-safety so callers don't guard emit sites.
func NewNotifier(hub Broadcaster, recipients RecipientsFunc) *Notifier {
	return &Notifier{hub: hub, recipients: recipients}
}

// Publish broadcasts upd to its audience. In the fan-out shape a
// resolution failure drops the event with a log — fail closed: a missed
// live update degrades to the next poll/profile cycle, while falling
// back to an org-wide broadcast would re-open the cross-team disclosure
// this scoping exists to prevent.
func (n *Notifier) Publish(ctx context.Context, orgID string, upd Update) {
	if n == nil || n.hub == nil {
		return
	}
	if n.recipients == nil {
		n.hub.Broadcast(websocket.Event{Type: EventType, OrgID: orgID, Data: upd})
		return
	}
	owner, repo, ok := strings.Cut(upd.ID, "/")
	if !ok {
		repoeventLog.Error("dropping event with malformed repo id", "id", upd.ID)
		return
	}
	userIDs, err := n.recipients(ctx, orgID, owner, repo)
	if err != nil {
		repoeventLog.Error("resolve recipients failed; dropping event", "repo", upd.ID, "error", err)
		return
	}
	for _, uid := range userIDs {
		n.hub.Broadcast(websocket.Event{Type: EventType, OrgID: orgID, UserID: uid, Data: upd})
	}
}
