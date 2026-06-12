package curator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Curator owns the per-project Claude Code chat session lifecycle.
// One instance per process; each project gets its own goroutine on
// first SendMessage. Cross-project messages run in parallel; same-
// project messages queue serially so the conversation history stays
// coherent.
//
// The per-project goroutine processes each turn under the requesting
// user's identity (curator_requests.creator_user_id), wrapping every
// store call in stores.Tx.SyntheticClaimsWithTx so multi-mode RLS
// policies on (org_id, creator_user_id) gate the writes. SKY-298.
type Curator struct {
	stores db.Stores
	wsHub  *websocket.Hub

	mu    sync.Mutex
	model string
	// SKY-389 per-org run-credential seam, wired once at startup via
	// SetRunCredentialResolvers. modelFor supersedes the process-global
	// model above; secrets feeds RunOptions.Secrets. Tests leave both nil
	// and fall back to model / the ambient-subscription path.
	//
	// ghResolver is the per-(org, owner) GitHub credential source used to
	// authenticate the host-side pinned-repo worktree refresh
	// (materializePinnedRepos → EnsureCuratorWorktree) for private repos in
	// multi mode. Nil in tests / when unset — the refresh then runs with no
	// injected credential (the unauthenticated path).
	secrets    agentproc.SecretsReader
	modelFor   func(context.Context, string, string) string
	ghResolver ghclient.Resolver
	sessions   map[string]*projectSession // projectID → goroutine handle

	// closed is set during Shutdown; SendMessage rejects after this.
	closed bool
}

// New constructs a Curator. Call
// stores.Curator.CancelOrphanedNonTerminalRequests at startup
// before constructing — see main.go wiring.
//
// stores carries the Tx runner (for SyntheticClaimsWithTx wraps), the
// CuratorStore (per-turn writes plus the admin-pool …System cancel
// variants the system-driven cleanup paths use), the ProjectStore
// (session-id bookkeeping), the PromptStore (skill materialization),
// and the RepoStore (pinned-repo materialization). Every row read/write
// the curator issues now goes through a claims-bound tx or an
// admin-pool door, so no raw *sql.DB handle is retained (TFAC-64).
func New(stores db.Stores, wsHub *websocket.Hub, model string) *Curator {
	return &Curator{
		stores:   stores,
		wsHub:    wsHub,
		model:    model,
		sessions: make(map[string]*projectSession),
	}
}

// SetRunCredentialResolvers wires the per-org run-credential seam: the GitHub
// credential resolver (used to authenticate private pinned-repo refreshes
// host-side), the per-org LLM-credential reader (SKY-389 — nil in
// local → ambient subscription; system-door reader in multi), and the
// per-(org, team) default-model resolver. Both modes resolve through these so
// credential resolution stops branching on mode. Set once at startup,
// post-New. Any may be nil; resolveModel falls back to the constructor model,
// a nil secrets reader yields the ambient-subscription fallback, and a nil
// resolver makes the pinned-repo refresh credential-free (the prior
// path). Signature mirrors Spawner.SetRunCredentialResolvers for symmetry.
func (c *Curator) SetRunCredentialResolvers(resolver ghclient.Resolver, secrets agentproc.SecretsReader, modelFor func(context.Context, string, string) string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ghResolver = resolver
	c.secrets = secrets
	c.modelFor = modelFor
}

// cloneTokenFor resolves the App installation token for a host-side fetch of
// a pinned repo owned by owner, via the GitHub resolver. Multi-mode only, to
// match the spawner (Spawner.resolveCloneToken) and keep local pinned-repo
// refreshes on their existing path (operator SSH key / anonymous HTTPS) —
// local behavior is unchanged by this ticket. Returns "" when local, when no
// resolver is wired, or when resolution fails; the refresh then runs with no
// injected credential. Logged-not-fatal: the refresh is already best-effort
// per repo.
func (c *Curator) cloneTokenFor(ctx context.Context, orgID, owner string) string {
	if runmode.Current() == runmode.ModeLocal {
		return ""
	}
	c.mu.Lock()
	resolver := c.ghResolver
	c.mu.Unlock()
	if resolver == nil {
		return ""
	}
	tok, err := resolver.TokenFor(ctx, orgID, owner)
	if err != nil {
		log.Printf("[curator] resolve clone token for org %s owner %q: %v (refresh proceeds unauthenticated)", orgID, owner, err)
		return ""
	}
	return tok.Value
}

// resolveModel resolves the project-owning team's default model via the
// SKY-389 resolver, falling back to the constructor-supplied model when no
// resolver is wired (tests) or the resolver returns empty. teamID is the
// project's owning team (a project belongs to exactly one team), so a
// multi-team org honors each team's model choice; empty falls back to the
// org default team inside the resolver.
func (c *Curator) resolveModel(ctx context.Context, orgID, teamID string) string {
	c.mu.Lock()
	fn := c.modelFor
	fallback := c.model
	c.mu.Unlock()
	if fn != nil {
		if m := fn(ctx, orgID, teamID); m != "" {
			return m
		}
	}
	return fallback
}

// getSecrets returns the per-org LLM-credential reader threaded into
// RunOptions.Secrets: nil in local (ambient subscription), the system-door
// reader in multi. SKY-389.
func (c *Curator) getSecrets() agentproc.SecretsReader {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.secrets
}

// queueItem carries everything the per-project goroutine needs to
// dispatch a turn under the requesting user's identity. orgID +
// creatorUserID are captured at enqueue time (SendMessage's handler
// context) so the goroutine doesn't have to read the curator_requests
// row again just to figure out who to bill the writes to — that read
// would itself need claims set under Postgres RLS, creating a chicken-
// and-egg problem. See SKY-298 routing notes.
type queueItem struct {
	requestID     string
	orgID         string
	creatorUserID string
}

// SendMessage records the user's input as a queued curator_request,
// hands it to the project's goroutine, and returns the request id.
// The HTTP handler returns 202 + this id; the per-project goroutine
// flips status to running on pickup and to terminal on completion.
//
// orgID + creatorUserID identify the requesting user — every write
// the goroutine produces for this turn (running flip, agent stream
// messages, pending-context consume/finalize/revert, terminal status)
// runs inside Stores.Tx.SyntheticClaimsWithTx with these claims set
// so multi-mode RLS attributes the rows correctly. In local mode the
// handler passes runmode.LocalDefaultOrgID + LocalDefaultUserID; the
// D9 sweep (SKY-253) will replace those with values from request
// context.
//
// The user's content is required (empty/whitespace-only is rejected
// at the handler before reaching us); the project must exist
// (handler checks). This function does not validate either —
// callers are trusted to pre-check.
//
// Shutdown safety: getOrStartSession holds c.mu and refuses to
// hand back a session once c.closed flips, so a SendMessage that
// races Shutdown either (a) wins the lock first and gets a session
// that Shutdown then tears down — the session ctx kills the dispatch
// before it spawns claude — or (b) loses the lock and gets nil back,
// in which case the persisted row is flipped to cancelled before we
// return. Either way, no message reaches a non-running goroutine.
func (c *Curator) SendMessage(ctx context.Context, projectID, orgID, creatorUserID, content string) (string, error) {
	var requestID string
	if err := c.stores.Tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, creatorUserID, content)
		if err != nil {
			return err
		}
		requestID = id
		return nil
	}); err != nil {
		return "", fmt.Errorf("create curator request: %w", err)
	}

	session := c.getOrStartSession(orgID, projectID)
	if session == nil {
		// Best-effort cancel via the admin-pool …System door — the
		// "curator is shut down" path runs from the handler goroutine
		// (not the per-project goroutine) with no synthetic-claims tx in
		// scope, so the RLS-gated app pool would reject the UPDATE and
		// leave the freshly created row dangling. TFAC-64.
		_, _ = c.stores.Curator.MarkRequestCancelledIfActiveSystem(ctx, orgID, requestID, "curator is shut down")
		return "", errors.New("curator is shut down")
	}

	item := queueItem{requestID: requestID, orgID: orgID, creatorUserID: creatorUserID}
	select {
	case session.queue <- item:
		c.broadcastRequestUpdate(orgID, projectID, requestID, "queued")
		return requestID, nil
	default:
		// Queue is full — should not happen at the per-project depth
		// we configure, but if it ever does, fail the row up-front
		// rather than blocking the HTTP handler. Admin-pool …System door
		// for the same no-claims reason as the shutdown path above. TFAC-64.
		_, _ = c.stores.Curator.CompleteRequestSystem(ctx, orgID, requestID, "failed", "curator queue full", 0, 0, 0)
		c.broadcastRequestUpdate(orgID, projectID, requestID, "failed")
		return "", errors.New("curator queue is full")
	}
}

// Cancel fires the per-project cancel func, terminating the in-flight
// agentproc.Run. The goroutine flips the row to cancelled when it
// observes ctx.Err(). Returns nil even if no in-flight goroutine
// exists — the typical race between user click and goroutine
// scheduling means "nothing to cancel" is a routine outcome rather
// than an error. Caller decides whether to surface it as 404 by
// checking InFlightCuratorRequestForProject first.
func (c *Curator) Cancel(projectID string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	c.mu.Unlock()
	if !ok {
		return
	}
	session.cancelInFlight()
}

// CancelProject is the project-delete hook: cancel any in-flight
// request, drain queued requests to cancelled (so the deleted
// project doesn't have ghost queued rows), and stop the goroutine
// so nothing runs after the project row is gone.
//
// Called BEFORE the projects DELETE so the FK cascade doesn't
// race a still-running goroutine. The DB cascade (curator_requests
// → curator_messages) takes care of the row removal once the
// project row is dropped.
//
// orgID is the project's owning tenant, threaded through to
// broadcast events so the cancellation toast/update only reaches
// connections authed against that org.
func (c *Curator) CancelProject(orgID, projectID string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	if ok {
		delete(c.sessions, projectID)
	}
	c.mu.Unlock()

	if !ok {
		// No goroutine ever started — but there may still be queued
		// rows from a previous process that the goroutine never got
		// a chance to drain. Cancel them at the DB level so the
		// FK cascade on project delete doesn't leave behind status
		// confusion.
		c.cancelQueuedRows(orgID, projectID, "project deleted")
		return
	}
	session.shutdown("project deleted")
	c.cancelQueuedRows(orgID, projectID, "project deleted")
}

// Shutdown stops every per-project goroutine and rejects further
// SendMessage calls. Called from main.go on graceful shutdown so
// in-flight CC subprocesses are SIGKILLed before the process exits.
// In-flight rows land as cancelled with reason "process shutting
// down"; queued rows are not resumed by the next process and will
// instead be cancelled on restart by orphaned-request cleanup.
func (c *Curator) Shutdown() {
	c.mu.Lock()
	c.closed = true
	sessions := c.sessions
	c.sessions = make(map[string]*projectSession)
	c.mu.Unlock()

	for _, s := range sessions {
		s.shutdown("process shutting down")
	}
}

// cancelQueuedRows flips never-picked-up queued curator_requests for a
// project to cancelled. Called from CancelProject (handler-side) and
// from the fallback path in SendMessage when the curator is shut down.
//
// System-driven cancel: there is no live request/JWT context here (the
// rows may have been enqueued by any user, possibly in a previous
// process), so both the list and the per-row cancel route through the
// admin-pool …System doors. Under multi-mode RLS the app pool would
// hide the queued rows from the SELECT and reject the UPDATE, leaving
// them dangling past the project FK cascade. The row already records
// its creator (creator_user_id) for audit; a system cancel isn't a user
// action, so it doesn't reconstruct per-user claims. orgID scopes the
// admin-pool query. TFAC-64.
func (c *Curator) cancelQueuedRows(orgID, projectID, reason string) {
	ctx := context.Background()
	queued, err := c.stores.Curator.QueuedRequestsForProjectSystem(ctx, orgID, projectID)
	if err != nil {
		log.Printf("[curator] warning: failed to list queued requests for project %s: %v", projectID, err)
		return
	}
	for _, req := range queued {
		flipped, err := c.stores.Curator.MarkRequestCancelledIfActiveSystem(ctx, orgID, req.ID, reason)
		if err != nil {
			log.Printf("[curator] warning: cancel queued request %s: %v", req.ID, err)
			continue
		}
		if flipped {
			c.broadcastRequestUpdate(orgID, projectID, req.ID, "cancelled")
		}
	}
}

// getOrStartSession returns the per-project session, starting a new
// goroutine if needed. Holding c.mu across the start prevents two
// concurrent SendMessage calls for the same project from spawning
// two goroutines, and folds the closed-check into the same critical
// section as the map mutation so a racing Shutdown can't observe a
// "no sessions to stop" snapshot while a fresh session is being
// inserted. Returns nil iff the curator has been shut down — caller
// flips the persisted row to cancelled so it doesn't dangle.
func (c *Curator) getOrStartSession(orgID, projectID string) *projectSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if existing, ok := c.sessions[projectID]; ok {
		return existing
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &projectSession{
		curator:   c,
		projectID: projectID,
		orgID:     orgID,
		queue:     make(chan queueItem, sessionQueueDepth),
		ctx:       ctx,
		stopAll:   cancel,
		done:      make(chan struct{}),
	}
	c.sessions[projectID] = session
	go session.run()
	return session
}

// sessionQueueDepth bounds how many user messages can be queued for
// one project ahead of the active one. Set generously for human-
// driven chat — a person can't reasonably backlog more than a
// handful of follow-ups before the answer to the first arrives.
const sessionQueueDepth = 64
