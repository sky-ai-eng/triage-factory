package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// agentHandler serves the agent-run endpoints (status / messages / cancel /
// message / interrupt / permissions / runs / artifact refresh). spawner and
// reconciler are read through getters so the handler always sees the current
// instance, (re)wired onto the server after construction.
type agentHandler struct {
	tx         db.TxRunner
	ws         *websocket.Hub
	spawner    func() *delegate.Spawner
	reconciler func() *reconcile.Reconciler
}

func (ag *agentHandler) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	var run *domain.AgentRun
	var resp map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		counts, e := tx.Artifacts.CountByRun(r.Context(), orgID, []string{run.ID})
		if e != nil {
			return e
		}
		resp = runResponse(r.Context(), tx.Artifacts, orgID, run, counts[run.ID])
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if run == nil {
		notFound(w, "run")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleArtifactRefresh is the Tier-2, run-scoped half of artifact
// reconciliation (TFAC-464): the run view polls it (~5s while open) to pull a
// run's non-terminal artifacts fresh against GitHub without waiting for the
// background per-org cycle. It runs the SAME reconcile path as Tier 1, bounded
// to this one run's artifacts, and pushes any transition over the WS hub.
//
// Authorization rides the request-claims read: AgentRuns.Get + Artifacts.ListByRun
// are RLS-scoped, so a user can only refresh a run their team can see. The GitHub
// calls and the artifact/memory writes happen OUTSIDE that read tx (no network
// I/O under a held transaction) — the reconciler writes via its admin-pool path.
func (ag *agentHandler) handleArtifactRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")

	rc := ag.reconciler()
	if rc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reconciler not ready"})
		return
	}

	// Read the run + its reconcilable non-terminal artifacts under request
	// claims (authorization + RLS scoping). Filter to the same working set the
	// org-wide Tier-1 query selects so the two tiers reconcile identically.
	var run *domain.AgentRun
	var arts []domain.Artifact
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil || run == nil {
			return e
		}
		all, e := tx.Artifacts.ListByRun(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		for _, a := range all {
			if domain.IsReconcilableNonTerminal(a.Kind, a.State) {
				arts = append(arts, a)
			}
		}
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if run == nil {
		notFound(w, "run")
		return
	}

	updated, err := rc.Reconcile(r.Context(), orgID, arts)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(updated)})
}

// artifactJSON is the wire shape of one run artifact for
// GET /api/agent/runs/{runID}/artifacts — the run's artifacts as the board /
// run-detail UI (TFAC-470) consumes them, across every kind (branch /
// pull_request / review / issue / comment). details is the PARSED details_json
// (the kind-specific payload as a JSON object, or null when absent/unparseable)
// so the client gets structured data, not a string to re-parse. The mutable
// dedup_key / org_id / team_id internals stay off the wire.
type artifactJSON struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Provider   string          `json:"provider"`
	State      string          `json:"state"`
	Target     string          `json:"target"`
	ExternalID string          `json:"external_id"`
	URL        string          `json:"url"`
	Details    json.RawMessage `json:"details"`
	CreatedAt  time.Time       `json:"created_at"`
}

// toArtifactJSON projects a stored artifact onto the read-API wire shape.
func toArtifactJSON(a domain.Artifact) artifactJSON {
	return artifactJSON{
		ID:         a.ID,
		Kind:       a.Kind,
		Provider:   a.Provider,
		State:      a.State,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		Details:    rawDetails(a.DetailsJSON),
		CreatedAt:  a.CreatedAt,
	}
}

// rawDetails returns details_json as embeddable JSON, or nil (which marshals
// to null) when it's empty or not valid JSON. The guard matters: json.Marshal
// validates a json.RawMessage, so handing it a malformed payload would fail the
// whole response — a corrupt details_json on one row must not 500 the list.
func rawDetails(detailsJSON string) json.RawMessage {
	if detailsJSON == "" || !json.Valid([]byte(detailsJSON)) {
		return nil
	}
	return json.RawMessage(detailsJSON)
}

// handleAgentArtifacts returns every artifact a run produced — branch, PR,
// review, issue, comment — newest first (A·6, TFAC-465). It reuses
// Artifacts.ListByRun and reads the run first under request claims, so a run
// the caller's team can't see is a 404 (RLS scopes both reads), never a leak;
// a member of the owning team gets the list. Backs the run-detail surface
// (TFAC-470); team-level aggregation is TFAC-449 C2, not here.
func (ag *agentHandler) handleAgentArtifacts(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")

	var run *domain.AgentRun
	var arts []domain.Artifact
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil || run == nil {
			return e
		}
		arts, e = tx.Artifacts.ListByRun(r.Context(), orgID, runID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if run == nil {
		notFound(w, "run")
		return
	}

	out := make([]artifactJSON, len(arts))
	for i, a := range arts {
		out[i] = toArtifactJSON(a)
	}
	writeJSON(w, http.StatusOK, out)
}

// runResponse projects an AgentRun into the wire shape the frontend
// consumes, augmented with `artifact_count` (so the Board card can show how
// many artifacts a run produced without a per-card fetch) and, when the run is
// parked, `pending_kind` + `pending_artifact_id` so the page can pick the right
// approval card variant ("Review" or "Open PR") and address the overlay by the
// gating artifact's id. A run parks either on a finalized pending review (a
// review artifact whose ready sentinel is set) or a draft PR (a pull_request
// artifact in state=draft); the discriminator has to come from the server
// because the run row itself doesn't know which kind queued it.
//
// artifactCount is supplied by the caller (computed via Artifacts.CountByRun)
// rather than derived here, so the run-list path can batch one count query for
// every run instead of an N+1 of per-run reads. pending_kind still needs the
// actual artifacts, so it keeps its own by-run read — a no-op on the common
// terminal case, and bounded because parked runs are few. That read's error
// swallows + logs: pending_kind is informational for the UI and an erroring
// lookup shouldn't fail the whole status fetch.
func runResponse(ctx context.Context, artifacts db.ArtifactStore, orgID string, run *domain.AgentRun, artifactCount int) map[string]any {
	out := map[string]any{
		"ID":                   run.ID,
		"TaskID":               run.TaskID,
		"PromptID":             run.PromptID,
		"Status":               run.Status,
		"Model":                run.Model,
		"StartedAt":            run.StartedAt,
		"CompletedAt":          run.CompletedAt,
		"TotalCostUSD":         run.TotalCostUSD,
		"DurationMs":           run.DurationMs,
		"NumTurns":             run.NumTurns,
		"StopReason":           run.StopReason,
		"WorktreePath":         run.WorktreePath,
		"ResultSummary":        run.ResultSummary,
		"SessionID":            run.SessionID,
		"MemoryMissing":        run.MemoryMissing,
		"TriggerType":          run.TriggerType,
		"TriggerID":            run.TriggerID,
		"blueprint_run_id":     run.BlueprintRunID,
		"blueprint_step_index": run.BlueprintStepIndex,
		"artifact_count":       artifactCount,
	}
	// pending_kind only relevant when the run is parked. Both overlays are
	// addressed by the gating artifact's id (pending_artifact_id): a finalized
	// review opens ReviewOverlay, a draft PR opens the PR overlay. A review that
	// was started but never submitted has no ready sentinel and does NOT park, so
	// FirstReadyReview (not a bare pending check) gates the review case.
	if run.Status == "pending_approval" {
		if arts, err := artifacts.ListByRun(ctx, orgID, run.ID); err == nil {
			if rv := domain.FirstReadyReview(arts); rv != nil {
				out["pending_kind"] = "review"
				out["pending_artifact_id"] = rv.ID
			} else if pr := domain.FirstDraftPullRequest(arts); pr != nil {
				out["pending_kind"] = "pr"
				out["pending_artifact_id"] = pr.ID
			}
		}
	}
	return out
}

func (ag *agentHandler) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	var messages []domain.AgentMessage
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		messages, e = tx.AgentRuns.Messages(r.Context(), orgID, runID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if messages == nil {
		messages = []domain.AgentMessage{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (ag *agentHandler) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := spawner.Cancel(orgID, runID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// runVisible reports whether runID exists and is visible to the caller's org
// (and team, under RLS). The steering control ops resolve a run by id against
// in-memory state (the process registry, the permission broker), so this is the
// authorization gate that stops a known run id from one tenant being acted on by
// another: a non-existent or not-visible run reads as false → the caller 404s.
func (ag *agentHandler) runVisible(ctx context.Context, orgID, userID, runID string) (bool, error) {
	var exists bool
	err := ag.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		run, e := tx.AgentRuns.Get(ctx, orgID, runID)
		if e != nil {
			return e
		}
		exists = run != nil
		return nil
	})
	return exists, err
}

// handleAgentMessage records a free-form user message on a run, broadcasts it
// to watchers, then routes it: a live run is steered in place, an `open` run is
// woken via resume. The run is read in the same tx that records the message, so
// a run not visible to the caller's org reads as 404 (the authz gate before any
// control op) and an existing run's message is recorded before routing — the
// transcript stays optimistic (the user's words show immediately). A run that
// can take no message (terminal / no live process) is 409.
func (ag *agentHandler) handleAgentMessage(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")

	var body struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		badRequest(w, "text is required")
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	// Read + record in one tx: the Get authorizes against the run under the
	// caller's org (RLS), so a missing / cross-org run is a 404 before SendMessage
	// reaches the registry; an existing run gets the user message persisted (with
	// the row id carried back onto msg.ID for the broadcast's client dedup).
	msg := &domain.AgentMessage{RunID: runID, Role: "user", Subtype: "text", Content: body.Text}
	var runExists bool
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		run, e := tx.AgentRuns.Get(r.Context(), orgID, runID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		runExists = true
		id, e := tx.AgentRuns.InsertMessage(r.Context(), orgID, msg)
		if e != nil {
			return e
		}
		msg.ID = int(id)
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if !runExists {
		notFound(w, "run")
		return
	}
	ag.ws.Broadcast(websocket.Event{
		Type:  "agent_message",
		OrgID: orgID,
		RunID: runID,
		Data:  msg,
	})

	if err := spawner.SendMessage(r.Context(), orgID, runID, userID, body.Text); err != nil {
		writeJSON(w, steerErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// handleAgentInterrupt stops a live run's current turn, leaving the process
// alive for further input. The run is authorized under the caller's org first
// (404 if not visible) — the process registry is keyed by run id alone, so this
// gate keeps a known run id from interrupting another tenant's run. An existing
// run with no live process is 409 — nothing to interrupt.
func (ag *agentHandler) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	exists, err := ag.runVisible(r.Context(), orgID, userID, runID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "run")
		return
	}
	if err := spawner.Interrupt(r.Context(), runID); err != nil {
		writeJSON(w, steerErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "interrupted"})
}

// steerErrorStatus maps a SendMessage / Interrupt error to an HTTP status. A
// run that can't take the op right now — no live process (ErrNoLiveProcess),
// not steerable (ErrRunNotSteerable), or a lost resume race
// (ErrRunNotResumable) — is 409 Conflict so the client refreshes and re-reads
// the run's state. An expired workspace (ErrWorkspaceExpired) is 410 Gone: the
// run's saved state was reaped after the retention window, so retrying won't
// help — the client surfaces the clear error rather than a transient conflict.
// Everything else is a server-side 500.
func steerErrorStatus(err error) int {
	switch {
	case errors.Is(err, delegate.ErrWorkspaceExpired):
		return http.StatusGone
	case errors.Is(err, delegate.ErrNoLiveProcess),
		errors.Is(err, delegate.ErrRunNotSteerable),
		errors.Is(err, delegate.ErrRunNotResumable):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// handleAgentPermission answers a pending tool-permission prompt a run surfaced
// via a `permission_request` WS event. Body: {"behavior":"allow"|"deny",
// "message"?:string,"updated_input"?:object}. The run is authorized under the
// caller's org (RLS) first — like the message/interrupt endpoints — so a run not
// visible to this org is 404; a request that isn't pending (already answered,
// timed out, or never existed) is also 404.
func (ag *agentHandler) handleAgentPermission(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	runID := r.PathValue("runID")
	requestID := r.PathValue("requestID")

	var body struct {
		Behavior     string         `json:"behavior"`
		Message      string         `json:"message"`
		UpdatedInput map[string]any `json:"updated_input"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		badRequest(w, `behavior must be "allow" or "deny"`)
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}

	// Authorize the run under the caller's org before touching the broker — the
	// broker's own org check is a backstop, not a team-level RLS gate.
	exists, err := ag.runVisible(r.Context(), orgID, userID, runID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "run")
		return
	}

	err = spawner.ResolvePermission(orgID, runID, requestID, agentproc.PermissionDecision{
		Behavior:     body.Behavior,
		Message:      body.Message,
		UpdatedInput: body.UpdatedInput,
	})
	switch {
	case errors.Is(err, delegate.ErrNoPendingPermission):
		notFound(w, "permission request")
	case err != nil:
		internalError(w, "agent", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
	}
}

func (ag *agentHandler) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id query parameter required"})
		return
	}
	var runs []domain.AgentRun
	var out []map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		runs, e = tx.AgentRuns.ListForTask(r.Context(), orgID, taskID)
		if e != nil {
			return e
		}
		if runs == nil {
			runs = []domain.AgentRun{}
		}
		// Batch the per-run artifact_count into one query — the list path is
		// N+1-sensitive (Board's useWebSocket re-fetches it on every status
		// transition), so a per-run count read would scale with the task's run
		// history. CountByRun returns a runID→count map; a run with no
		// artifacts is simply absent (→ 0).
		runIDs := make([]string, len(runs))
		for i := range runs {
			runIDs[i] = runs[i].ID
		}
		counts, e := tx.Artifacts.CountByRun(r.Context(), orgID, runIDs)
		if e != nil {
			return e
		}
		// Project each run through runResponse so pending_kind rides
		// alongside on the list endpoint too. Board's useWebSocket calls
		// this on every status transition; without the discriminator on
		// the list response the Open-PR vs Review button choice would
		// flicker on first paint and only settle after the per-run fetch.
		out = make([]map[string]any, len(runs))
		for i := range runs {
			out[i] = runResponse(r.Context(), tx.Artifacts, orgID, &runs[i], counts[runs[i].ID])
		}
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
