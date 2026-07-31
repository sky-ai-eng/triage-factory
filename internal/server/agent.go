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
	conversationID := r.PathValue("conversationID")
	var run *domain.Conversation
	var resp map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
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
		// Derive has_unresolved_artifacts (+ per-kind counts) from the run's
		// artifact set. Only read the artifacts when the run has any —
		// counts==0 means there's nothing unresolved, so skip the list. A list
		// failure is best-effort: it omits the derived flags (logged) but must not
		// fail the status fetch or touch the authoritative count above.
		var arts []domain.Artifact
		if counts[run.ID] > 0 {
			if a, lerr := tx.Artifacts.ListByRun(r.Context(), orgID, run.ID); lerr != nil {
				serverLog.Warn("artifact lookup for has_unresolved_artifacts failed; omitting it (artifact_count unaffected)", "run", run.ID, "error", lerr)
			} else {
				arts = a
			}
		}
		resp = runResponse(run, counts[run.ID], arts)
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
// Authorization rides the request-claims read: Conversations.Get + Artifacts.ListByRun
// are RLS-scoped, so a user can only refresh a run their team can see. The GitHub
// calls and the artifact/memory writes happen OUTSIDE that read tx (no network
// I/O under a held transaction) — the reconciler writes via its admin-pool path.
func (ag *agentHandler) handleArtifactRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID := r.PathValue("conversationID")

	rc := ag.reconciler()
	if rc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reconciler not ready"})
		return
	}

	// Read the run + its reconcilable non-terminal artifacts under request
	// claims (authorization + RLS scoping). Filter to the same working set the
	// org-wide Tier-1 query selects so the two tiers reconcile identically.
	var run *domain.Conversation
	var arts []domain.Artifact
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil || run == nil {
			return e
		}
		all, e := tx.Artifacts.ListByRun(r.Context(), orgID, conversationID)
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
// GET /api/agent/conversations/{conversationID}/artifacts — the run's artifacts as the board /
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

// toArtifactJSON projects a stored artifact onto the read-API wire shape,
// emitting details_json as embedded JSON (or null when absent/corrupt).
func toArtifactJSON(a domain.Artifact) artifactJSON {
	var details json.RawMessage
	switch {
	case a.DetailsJSON == "":
		// No details — leave nil, which marshals to null.
	case json.Valid([]byte(a.DetailsJSON)):
		details = json.RawMessage(a.DetailsJSON)
	default:
		// Corrupt payload: serve null rather than fail the whole response's
		// marshal (json.Marshal validates a RawMessage). details_json is written
		// by our own marshaled structs, so an invalid value is a real upstream
		// bug worth a trace, not an expected empty — mirror the artifact handler's
		// "details unparseable" warning so it isn't a silent drop.
		artifactsLog.Warn("artifact details_json is not valid JSON; serving null",
			"artifact", a.ID, "dedup_key", a.DedupKey)
	}
	return artifactJSON{
		ID:         a.ID,
		Kind:       a.Kind,
		Provider:   a.Provider,
		State:      a.State,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		Details:    details,
		CreatedAt:  a.CreatedAt,
	}
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
	conversationID := r.PathValue("conversationID")

	var run *domain.Conversation
	var arts []domain.Artifact
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		run, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		arts, e = tx.Artifacts.ListByRun(r.Context(), orgID, conversationID)
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

// runResponse projects a Conversation into the wire shape the frontend consumes,
// augmented with `artifact_count` (so the Board card can show how many artifacts
// a run produced without a per-card fetch) and the derived approval signal:
// `has_unresolved_artifacts` (bool), `unresolved_pr_count` /
// `unresolved_review_count`, and `pending_artifact_ids` (the set of unresolved
// approvable artifact ids — all draft PRs + all ready reviews — for the per-item
// resolve UI). These replace the legacy `pending_kind` /
// `pending_artifact_id` overlay discriminators — approval is no longer a stored
// run status but a view over the unresolved-artifact set.
//
// Pure projection — it does no I/O. The caller supplies both inputs, which is
// what lets the run-list path batch its reads instead of issuing them per run:
//   - artifactCount from Artifacts.CountByRun (the single-run path counts one
//     run; the list path batches every run in one query — N+1 avoidance).
//   - arts is the run's artifact set, which the caller reads best-effort (only
//     for runs that have any artifact, so a run with none costs no list).
//
// The derived approval keys are emitted only when the answer is definitive: when
// the run has no artifacts (artifactCount == 0, so nothing can be unresolved) or
// when the artifact set is actually in hand (len(arts) > 0). When artifactCount
// is positive but arts is empty — a best-effort read failed — the keys are
// OMITTED rather than reported as a misleading false, so a transient DB/RLS hiccup
// can't hide approval-required work; the client treats their absence as "unknown"
// and re-derives on the next refresh.
func runResponse(run *domain.Conversation, artifactCount int, arts []domain.Artifact) map[string]any {
	out := map[string]any{
		"ID":                   run.ID,
		"TaskID":               run.TaskID,
		"PromptID":             run.PromptID,
		"Status":               run.Status,
		"Model":                run.Model,
		"StartedAt":            run.StartedAt,
		"QueuedAt":             run.QueuedAt,
		"ClaimedAt":            run.ClaimedAt,
		"CompletedAt":          run.CompletedAt,
		"TotalCostUSD":         run.TotalCostUSD,
		"DurationMs":           run.DurationMs,
		"NumTurns":             run.NumTurns,
		"StopReason":           run.StopReason,
		"WorktreePath":         run.WorktreePath,
		"ResultSummary":        run.ResultSummary,
		"FailureKind":          string(run.FailureKind),
		"SessionID":            run.SessionID,
		"MemoryMissing":        run.MemoryMissing,
		"TriggerType":          run.TriggerType,
		"TriggerID":            run.TriggerID,
		"actor_agent_id":       run.ActorAgentID,
		"actor_agent_name":     run.ActorAgentName,
		"blueprint_run_id":     run.BlueprintRunID,
		"blueprint_step_index": run.BlueprintStepIndex,
		"artifact_count":       artifactCount,
	}
	if artifactCount == 0 || len(arts) > 0 {
		prCount, reviewCount := domain.UnresolvedArtifactCounts(arts)
		out["has_unresolved_artifacts"] = prCount > 0 || reviewCount > 0
		out["unresolved_pr_count"] = prCount
		out["unresolved_review_count"] = reviewCount
		// The set of unresolved approvable artifact ids (all draft PRs + all ready
		// reviews) — what the approval UI lists for per-item resolve. Emitted under
		// the same definitive-only guard as the counts above, and always non-nil so
		// the field is [] rather than null when nothing is unresolved.
		out["pending_artifact_ids"] = domain.UnresolvedArtifactIDs(arts)
	}
	return out
}

func (ag *agentHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID := r.PathValue("conversationID")
	var messages []domain.Message
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		messages, e = tx.Conversations.Messages(r.Context(), orgID, conversationID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, domain.MessageDTOs(messages))
}

func (ag *agentHandler) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID := r.PathValue("conversationID")
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	if err := spawner.Cancel(orgID, conversationID, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// runVisible reports whether conversationID exists and is visible to the caller's org
// (and team, under RLS). The steering control ops resolve a run by id against
// in-memory state (the process registry, the permission broker), so this is the
// authorization gate that stops a known run id from one tenant being acted on by
// another: a non-existent or not-visible run reads as false → the caller 404s.
func (ag *agentHandler) runVisible(ctx context.Context, orgID, userID, conversationID string) (bool, error) {
	var exists bool
	err := ag.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		run, e := tx.Conversations.Get(ctx, orgID, conversationID)
		if e != nil {
			return e
		}
		exists = run != nil
		return nil
	})
	return exists, err
}

// handleMessage records a free-form user message on a run, broadcasts it
// to watchers, then routes it: a live run is steered in place, an `open` run is
// woken via resume. The run is read in the same tx that records the message, so
// a run not visible to the caller's org reads as 404 (the authz gate before any
// control op) and an existing run's message is recorded before routing — the
// transcript stays optimistic (the user's words show immediately). A run that
// can take no message (terminal / no live process) is 409.
func (ag *agentHandler) handleMessage(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID := r.PathValue("conversationID")

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
	//
	// SDK runs only. A native conversation's one input door is the messages
	// table itself: SendMessage's native branch queues the row (pending) and
	// broadcasts it, so an optimistic insert here would put the same words in
	// front of the model twice — once as a bare user turn, once in the steer
	// envelope. The SDK path keeps the optimistic row because its live steer
	// injects into the process without writing one.
	msg := &domain.Message{ConversationID: conversationID, Role: "user", Content: body.Text}
	var runExists, nativeRun bool
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		run, e := tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if run == nil {
			return nil
		}
		runExists = true
		nativeRun = run.Runtime == domain.ConversationRuntimeNative
		if nativeRun {
			return nil
		}
		id, e := tx.Conversations.InsertMessage(r.Context(), orgID, msg)
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
	if !nativeRun {
		ag.ws.Broadcast(websocket.Event{
			Type:           "message",
			OrgID:          orgID,
			ConversationID: conversationID,
			Data:           msg.ToDTO(),
		})
	}

	if err := spawner.SendMessage(r.Context(), orgID, conversationID, userID, body.Text); err != nil {
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
	conversationID := r.PathValue("conversationID")
	spawner := ag.spawner()
	if spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delegation not configured"})
		return
	}
	exists, err := ag.runVisible(r.Context(), orgID, userID, conversationID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "run")
		return
	}
	if err := spawner.Interrupt(r.Context(), conversationID); err != nil {
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
// A cross-pod signal whose owning executor never acked (ErrSignalAckTimeout,
// TFAC-585) is 504 Gateway Timeout — the reply-leg contract's "run owner did
// not acknowledge; the run may be mid-teardown" case; the UI already
// tolerates steer/interrupt failure. Everything else is a server-side 500.
func steerErrorStatus(err error) int {
	switch {
	case errors.Is(err, delegate.ErrWorkspaceExpired):
		return http.StatusGone
	case errors.Is(err, delegate.ErrSignalAckTimeout):
		return http.StatusGatewayTimeout
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
	conversationID := r.PathValue("conversationID")
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
	exists, err := ag.runVisible(r.Context(), orgID, userID, conversationID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "run")
		return
	}

	err = spawner.ResolvePermission(orgID, conversationID, requestID, agentproc.PermissionDecision{
		Behavior:     body.Behavior,
		Message:      body.Message,
		UpdatedInput: body.UpdatedInput,
	})
	switch {
	case errors.Is(err, delegate.ErrNoPendingPermission):
		notFound(w, "permission request")
	case errors.Is(err, delegate.ErrSignalAckTimeout):
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
	case err != nil:
		internalError(w, "agent", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
	}
}

// enrichRuns projects runs onto the wire shape, augmenting each with a batched
// artifact_count and the derived has_unresolved_artifacts (+ per-kind counts).
// Both artifact reads stay O(1) in queries regardless of how many runs the set
// has. Board's useWebSocket re-fetches on every status transition, so a per-run
// read would scale with the task's run history.
//
//   - artifact_count for every run: one CountByRun (conversationID→count; a run with no
//     artifacts is absent → 0).
//   - has_unresolved_artifacts for the runs that have artifacts: one ListByRuns
//     over just those run ids (count>0), grouped per run. The derivation needs
//     the actual artifacts, but a run with none can't have an unresolved one, so
//     it costs no list.
//
// Best-effort: a ListByRuns failure drops the derived flags (logged) but leaves
// counts and the rest intact. Shared by the single-task and batched (task_ids)
// run-list paths.
func enrichRuns(ctx context.Context, tx db.TxStores, orgID string, runs []domain.Conversation) ([]map[string]any, error) {
	runIDs := make([]string, len(runs))
	for i := range runs {
		runIDs[i] = runs[i].ID
	}
	counts, err := tx.Artifacts.CountByRun(ctx, orgID, runIDs)
	if err != nil {
		return nil, err
	}
	var withArtifacts []string
	for i := range runs {
		if counts[runs[i].ID] > 0 {
			withArtifacts = append(withArtifacts, runs[i].ID)
		}
	}
	artsByRun := map[string][]domain.Artifact{}
	if len(withArtifacts) > 0 {
		if arts, lerr := tx.Artifacts.ListByRuns(ctx, orgID, withArtifacts); lerr != nil {
			serverLog.Warn("artifact batch lookup failed; omitting has_unresolved_artifacts (artifact_count unaffected)", "error", lerr)
		} else {
			for _, a := range arts {
				artsByRun[a.ConversationID] = append(artsByRun[a.ConversationID], a)
			}
		}
	}
	out := make([]map[string]any, len(runs))
	for i := range runs {
		out[i] = runResponse(&runs[i], counts[runs[i].ID], artsByRun[runs[i].ID])
	}
	return out, nil
}

// splitCommaList splits a comma-separated query value into trimmed,
// de-duplicated, non-empty ids, preserving first-seen order.
func splitCommaList(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func (ag *agentHandler) handleConversations(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	// Batched path: ?task_ids=a,b,c[&include=messages] returns runs keyed by
	// task id (and, when requested, each task's primary-run transcript keyed by
	// run id) in one payload — the Board's per-refresh fan-out collapsed from
	// O(tasks) serial round-trips to one. The single ?task_id path below is
	// unchanged for back-compat.
	if raw := r.URL.Query().Get("task_ids"); raw != "" {
		ag.handleConversationsBatched(w, r, orgID, userID, raw)
		return
	}

	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id or task_ids query parameter required"})
		return
	}
	var out []map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		runs, e := tx.Conversations.ListForTask(r.Context(), orgID, taskID)
		if e != nil {
			return e
		}
		if runs == nil {
			runs = []domain.Conversation{}
		}
		out, e = enrichRuns(r.Context(), tx, orgID, runs)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// maxBatchTaskIDs caps how many task ids one batched run-list request
// processes, bounding the DB work and response size a single call can trigger.
// 500 matches the store's IN-list chunk size, so the common board (well under
// it) is one chunk per read; a larger board is truncated (see the handler) and
// logged rather than allowed to fan out unbounded.
const maxBatchTaskIDs = 500

// handleConversationsBatched serves GET /api/agent/conversations?task_ids=a,b,c. The
// response is { "runs": { <taskID>: []run }, "messages": { <conversationID>: []message } }:
// runs grouped per task (newest-first, same per-run projection as the single
// path), and — when include=messages — the transcript of each task's PRIMARY
// (newest-started) run, keyed by that run's id. The primary run is runs[0] per
// task, matching the single-task path's latestRun; this is exactly the data the
// Board seeds onto each card, now in one round-trip instead of 2–3 per task.
// `messages` is omitted unless include=messages.
//
// Reading every task in one WithTx also makes the snapshot internally
// consistent: the old per-task serial loop read each task at a different
// transaction boundary, so a status change mid-refresh could return some tasks
// in the old state and some in the new. One tx removes that flicker class.
func (ag *agentHandler) handleConversationsBatched(w http.ResponseWriter, r *http.Request, orgID, userID, raw string) {
	taskIDs := splitCommaList(raw)
	if len(taskIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_ids must contain at least one id"})
		return
	}
	if len(taskIDs) > maxBatchTaskIDs {
		// Bound the DB work / payload one request can trigger. The board sends
		// its visible tasks in column order (active columns first, then done —
		// which is 7-day- but not count-capped), so truncating the tail drops
		// the oldest done cards' run enrichment (they just miss a run badge
		// until a WS update), never the active work. Logged so an oversized
		// board is observable. This is the request bound; the SQLite
		// variable-limit safety is separate (the store reads chunk regardless).
		serverLog.Warn("batched run-list task_ids exceeded cap; truncating",
			"requested", len(taskIDs), "cap", maxBatchTaskIDs)
		taskIDs = taskIDs[:maxBatchTaskIDs]
	}
	includeMessages := false
	for _, inc := range splitCommaList(r.URL.Query().Get("include")) {
		if inc == "messages" {
			includeMessages = true
		}
	}

	runsByTask := map[string][]map[string]any{}
	messagesByRun := map[string][]domain.MessageDTO{}
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		runs, e := tx.Conversations.ListForTasks(r.Context(), orgID, taskIDs)
		if e != nil {
			return e
		}
		// One enrichRuns over the whole flat slice keeps the artifact reads
		// O(1) regardless of task/run count; the index alignment lets us group
		// the projected responses by task without re-projecting.
		enriched, e := enrichRuns(r.Context(), tx, orgID, runs)
		if e != nil {
			return e
		}
		var primaryRunIDs []string
		seenTask := map[string]bool{}
		for i := range runs {
			tid := runs[i].TaskID
			runsByTask[tid] = append(runsByTask[tid], enriched[i])
			// runs come back started_at DESC, so the first seen per task is its
			// primary (newest) run — the one whose transcript the card shows.
			if !seenTask[tid] {
				seenTask[tid] = true
				primaryRunIDs = append(primaryRunIDs, runs[i].ID)
			}
		}
		if includeMessages && len(primaryRunIDs) > 0 {
			msgs, e := tx.Conversations.MessagesForRuns(r.Context(), orgID, primaryRunIDs)
			if e != nil {
				return e
			}
			for _, m := range msgs {
				messagesByRun[m.ConversationID] = append(messagesByRun[m.ConversationID], m.ToDTO())
			}
		}
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}

	resp := map[string]any{"runs": runsByTask}
	if includeMessages {
		resp["messages"] = messagesByRun
	}
	writeJSON(w, http.StatusOK, resp)
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
