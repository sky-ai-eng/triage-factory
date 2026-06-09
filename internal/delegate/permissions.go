// The browser tool-permission round-trip. When a run is spawned with the
// handler this file builds, every canUseTool prompt the agent raises is
// surfaced to the browser as a `permission_request` WS event and the agent's
// turn is parked until the user answers via ResolvePermission (the
// POST .../permissions/{id} endpoint) — or a bounded timeout denies it. The
// broker (s.permPending) is in-memory only and keyed by the SDK-generated
// request id.
//
// Both runLiveAndDrive call sites (the initial run and the resume) pass this
// handler, so every run surfaces off-allowlist tools to the browser; the
// RunStation dock renders the prompt and posts the answer back through
// ResolvePermission.

package delegate

import (
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// ErrNoPendingPermission is returned by ResolvePermission when no in-flight
// request matches (orgID, runID, requestID) — it was already answered, timed
// out, or never existed. The permission endpoint maps it to 404.
var ErrNoPendingPermission = errors.New("no pending permission request")

// pendingPermission is one in-flight browser permission prompt: the 1-buffered
// channel the handler goroutine is parked on, plus the owning org so a resolve
// from another tenant can't satisfy it. The owning run is encoded in the broker
// key (see permKey), not stored here.
type pendingPermission struct {
	ch    chan agentproc.PermissionDecision
	orgID string
}

// permKey is the broker key for a pending prompt. The wrapper's request_id is
// only unique within a single run process (`perm-N` from a per-process
// counter), so two concurrent runs can collide on the same id — keying by
// (runID, requestID) keeps each run's prompts isolated. The NUL separator can't
// appear in a run uuid or a `perm-N` id, so the composite is unambiguous.
func permKey(runID, requestID string) string {
	return runID + "\x00" + requestID
}

// permTimeout is how long a surfaced prompt waits for an answer before denying.
// It is kept strictly below idleTimeout() so a pending prompt always resolves
// before the run could idle-hibernate mid-prompt (which would tear the warm
// process down with the reader goroutine still parked in the handler). Half the
// idle window is a clean strictly-less value for any idle, including the short
// ones tests inject.
func (s *Spawner) permTimeout() time.Duration {
	return s.idleTimeout() / 2
}

// BrowserPermissionHandler returns a PermissionHandler that surfaces each
// canUseTool prompt to the browser and parks the agent's turn until the user
// answers (ResolvePermission) or permTimeout() elapses (deny). Safe to call
// from the agentproc reader goroutine: it registers a 1-buffered channel keyed
// by the request id, broadcasts a `permission_request`, waits, and always
// deregisters.
func (s *Spawner) BrowserPermissionHandler(orgID, runID string) agentproc.PermissionHandler {
	return func(req agentproc.PermissionRequest) agentproc.PermissionDecision {
		key := permKey(runID, req.RequestID)
		ch := make(chan agentproc.PermissionDecision, 1)
		s.mu.Lock()
		s.permPending[key] = &pendingPermission{ch: ch, orgID: orgID}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.permPending, key)
			s.mu.Unlock()
		}()

		// Hub.Broadcast is nil-receiver-safe, so no guard is needed for the
		// hub-less test spawner.
		s.wsHub.Broadcast(websocket.Event{
			Type:  "permission_request",
			OrgID: orgID,
			RunID: runID,
			Data: map[string]any{
				"request_id": req.RequestID,
				"tool_name":  req.ToolName,
				"input":      req.Input,
			},
		})

		select {
		case d := <-ch:
			return d
		case <-time.After(s.permTimeout()):
			// Nobody answered before the bounded wait — deny so the turn resolves
			// ahead of idle-hibernate. A generous allowlist keeps this rare.
			return agentproc.PermissionDecision{Behavior: "deny", Message: "permission request timed out"}
		}
	}
}

// ResolvePermission delivers a decision to a pending permission request,
// unblocking the handler goroutine parked on it. The send is non-blocking (the
// channel is 1-buffered with a single receiver). A request that isn't pending —
// or whose pending entry was registered for a different org — is
// ErrNoPendingPermission for the caller to map to 404. The org check here is a
// broker-level backstop; the permission endpoint additionally authorizes the
// run under RLS (team-level) before calling this.
func (s *Spawner) ResolvePermission(orgID, runID, requestID string, d agentproc.PermissionDecision) error {
	s.mu.Lock()
	p, ok := s.permPending[permKey(runID, requestID)]
	s.mu.Unlock()
	if !ok || p.orgID != orgID {
		return ErrNoPendingPermission
	}
	select {
	case p.ch <- d:
		return nil
	default:
		// Slot already filled by a racing resolve — treat as no-longer-pending.
		return ErrNoPendingPermission
	}
}
