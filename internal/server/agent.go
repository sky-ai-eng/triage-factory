package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// agentHandler serves the agent-conversation endpoints (status / messages / cancel /
// message / interrupt / permissions / conversation list / artifact refresh). spawner and
// reconciler are read through getters so the handler always sees the current
// instance, (re)wired onto the server after construction.
type agentHandler struct {
	tx         db.TxRunner
	ws         *websocket.Hub
	spawner    func() *delegate.Spawner
	reconciler func() *reconcile.Reconciler
}

// conversationIDOr404 guards the {conversationID} path value — conversations.id
// is a uuid column on Postgres. See uuidPathOr404.
func conversationIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	return uuidPathOr404(w, r, "conversationID", "conversation")
}

// writeDelegationUnavailable answers the routes that need a wired spawner on a
// deployment that has none (delegation disabled). Server-side configuration,
// not a request fault, and not a transient upstream — 409 NOT_CONFIGURED, the
// same answer every other unconfigured-integration route gives.
func writeDelegationUnavailable(w http.ResponseWriter) {
	writeNotConfigured(w, "delegation is not configured on this deployment")
}

func (ag *agentHandler) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}
	ag.writeConversationResource(w, r, orgID, userID, conversationID)
}

// writeConversationResource reads the conversation resource exactly as
// handleAgentStatus does — the point read plus its display enrichment
// (artifact counts, blueprint step count, resumability) — and writes it as
// the response. Shared with handleAgentStop/handleMessage so a mutating verb
// route answers with the resource it just changed rather than a status stub
// naming only the field that verb happens to touch.
func (ag *agentHandler) writeConversationResource(w http.ResponseWriter, r *http.Request, orgID, userID, conversationID string) {
	var conv *domain.Conversation
	var resp map[string]any
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		counts, e := tx.Artifacts.CountByConversation(r.Context(), orgID, []string{conv.ID})
		if e != nil {
			return e
		}
		// Derive has_unresolved_artifacts (+ per-kind counts) from the conversation's
		// artifact set. Only read the artifacts when the conversation has any —
		// counts==0 means there's nothing unresolved, so skip the list. A list
		// failure is best-effort BY DESIGN — this is display enrichment on top
		// of an authoritative row, so it omits the derived flags (logged) but
		// must not fail the status fetch or touch the count above. The
		// swallowed-error sweep leaves it swallowed deliberately.
		var arts []domain.Artifact
		if counts[conv.ID] > 0 {
			if a, lerr := tx.Artifacts.ListByConversation(r.Context(), orgID, conv.ID); lerr != nil {
				serverLog.Warn("artifact lookup for has_unresolved_artifacts failed; omitting it (artifact_count unaffected)", "conversation", conv.ID, "error", lerr)
			} else {
				arts = a
			}
		}
		// The owning blueprint's plan length, so the conversation page can tell a
		// handed-off step from the one that ended the task. Best-effort like
		// the artifact list above: a failure (or a blueprint row RLS hides)
		// leaves the count at 0 and the projection unqualified, never a failed
		// status fetch.
		var stepCount int
		if conv.BlueprintRunID != "" {
			if lens, lerr := tx.Blueprints.StepPlanLengths(r.Context(), orgID, []string{conv.BlueprintRunID}); lerr != nil {
				serverLog.Warn("blueprint step-plan length lookup failed; omitting blueprint_step_count", "conversation", conv.ID, "blueprint_run", conv.BlueprintRunID, "error", lerr)
			} else {
				stepCount = lens[conv.BlueprintRunID]
			}
		}
		resp = conversationResponse(conv, counts[conv.ID], arts, stepCount, "")
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}
	addResumability(r.Context(), resp, orgID, conv, ag.spawner())
	writeJSON(w, http.StatusOK, resp)
}

// addResumability puts the server's answer to "can a follow-up land on this
// conversation?" on the conversation's detail read: `resumable`, plus
// `resume_blocked_reason` naming the rung that refused when it can't. It is the
// same walk SendMessage refuses on (Spawner.ResumabilityFor), so the composer
// the client renders and the send the server accepts cannot disagree — the
// client can see only one of the three inputs (status), and a stopped conversation whose
// workspace never made it looks identical from there to one that is warm.
//
// Detail read only, and deliberately: the board never shows a composer, and the
// workspace half of the answer is a blob existence check the batched list would
// pay per row for nothing.
//
// Two shapes of conversation are skipped, and their absence from the payload is the
// answer. An ACTIVE conversation is steered through its live process, a route this gate
// doesn't model — and the client's own `active` arm already opens the composer,
// so the check would be a blob read to confirm what the status said. A FAILED
// conversation has no coherent workspace by construction. In both cases the keys are
// omitted rather than guessed, and the client falls back to the status-only
// reading, which is right for both.
//
// Runs outside the tx that read the row: the workspace probe stats the disk and
// may reach blob storage, and no read should hold a transaction open across
// that. A spawner that isn't wired (delegation disabled) omits the keys too.
func addResumability(ctx context.Context, resp map[string]any, orgID string, conv *domain.Conversation, spawner *delegate.Spawner) {
	if spawner == nil || conv == nil {
		return
	}
	if domain.IsActiveConversationStatus(conv.Status) || conv.Status == domain.StatusFailed {
		return
	}
	ok, reason := spawner.ResumabilityFor(ctx, orgID, conv)
	resp["resumable"] = ok
	if !ok {
		resp["resume_blocked_reason"] = reason
	}
}

// handleArtifactRefresh is the Tier-2, conversation-scoped half of artifact
// reconciliation (TFAC-464): the conversation view polls it (~5s while open) to pull a
// conversation's non-terminal artifacts fresh against GitHub without waiting for the
// background per-org cycle. It runs the SAME reconcile path as Tier 1, bounded
// to this one conversation's artifacts, and pushes any transition over the WS hub.
//
// Authorization rides the request-claims read: Conversations.Get + Artifacts.ListByConversation
// are RLS-scoped, so a user can only refresh a conversation their team can see. The GitHub
// calls and the artifact/memory writes happen OUTSIDE that read tx (no network
// I/O under a held transaction) — the reconciler writes via its admin-pool path.
func (ag *agentHandler) handleArtifactRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}

	rc := ag.reconciler()
	if rc == nil {
		// Same reading as a missing spawner: this deployment runs no
		// reconciler, so the route has nothing to drive.
		writeNotConfigured(w, "artifact reconciliation is not configured on this deployment")
		return
	}

	// Read the conversation + its reconcilable non-terminal artifacts under request
	// claims (authorization + RLS scoping). Filter to the same working set the
	// org-wide Tier-1 query selects so the two tiers reconcile identically.
	var conv *domain.Conversation
	var arts []domain.Artifact
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil || conv == nil {
			return e
		}
		all, e := tx.Artifacts.ListByConversation(r.Context(), orgID, conversationID)
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
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	updated, err := rc.Reconcile(r.Context(), orgID, arts)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(updated)})
}

// artifactJSON is the wire shape of one conversation artifact for
// GET /api/agent/conversations/{conversationID}/artifacts — the conversation's artifacts as the board /
// RunDetail UI (TFAC-470) consumes them, across every kind (branch /
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
	out := artifactEnvelope(a)
	out.Details = storedArtifactDetails(a)
	return out
}

// artifactEnvelope fills the kind-independent half of the read shape, leaving
// Details to the caller. The single read (GET /api/artifacts/{id}) composes a
// live representation for two of the kinds, so it fills that slot itself
// rather than passing the stored payload through.
func artifactEnvelope(a domain.Artifact) artifactJSON {
	return artifactJSON{
		ID:         a.ID,
		Kind:       a.Kind,
		Provider:   a.Provider,
		State:      a.State,
		Target:     a.Target,
		ExternalID: a.ExternalID,
		URL:        a.URL,
		CreatedAt:  a.CreatedAt,
	}
}

// storedArtifactDetails is the details_json a row already holds, as the
// embedded JSON object it is.
func storedArtifactDetails(a domain.Artifact) json.RawMessage {
	switch {
	case a.DetailsJSON == "":
		// No details — nil, which marshals to null.
		return nil
	case json.Valid([]byte(a.DetailsJSON)):
		return json.RawMessage(a.DetailsJSON)
	default:
		// Corrupt payload: serve null rather than fail the whole response's
		// marshal (json.Marshal validates a RawMessage). details_json is written
		// by our own marshaled structs, so an invalid value is a real upstream
		// bug worth a trace, not an expected empty — mirror the artifact handler's
		// "details unparseable" warning so it isn't a silent drop.
		artifactsLog.Warn("artifact details_json is not valid JSON; serving null",
			"artifact", a.ID, "dedup_key", a.DedupKey)
		return nil
	}
}

// handleAgentArtifacts returns every artifact a conversation produced — branch, PR,
// review, issue, comment — newest first (A·6, TFAC-465). It reuses
// Artifacts.ListByConversation and reads the conversation first under request claims, so a
// conversation the caller's team can't see is a 404 (RLS scopes both reads), never a leak;
// a member of the owning team gets the list. Backs the conversation detail surface
// (TFAC-470); team-level aggregation is TFAC-449 C2, not here.
func (ag *agentHandler) handleAgentArtifacts(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}

	var conv *domain.Conversation
	var arts []domain.Artifact
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		arts, e = tx.Artifacts.ListByConversation(r.Context(), orgID, conversationID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	out := make([]artifactJSON, len(arts))
	for i, a := range arts {
		out[i] = toArtifactJSON(a)
	}
	writeJSON(w, http.StatusOK, out)
}

// conversationActionListRequest is the body of POST
// /api/agent/conversations/{conversationID}/actions/list. The conversation is
// the path id and the read has no filters of its own, so the body is paging
// alone.
type conversationActionListRequest struct {
	httpx.PageRequest
}

// handleAgentActions returns the external actions this conversation performed — the
// audit log of record, filtered to one conversation and newest first. The
// sibling of handleAgentArtifacts, and gated identically: the conversation is read first
// under request claims, so a conversation the caller's team can't see is a 404 rather
// than an empty list.
//
// A conversation's actions are bounded by nothing — an agent in a retry loop can append
// hundreds of rows — which is why this used to read a hardcoded 200 and the
// frontend held a copy of that number to know whether it had the whole answer.
// Both are gone: the page is the caller's to ask for and total_count says how
// many there are, so a truncated view is now impossible to mistake for a
// complete one.
//
// Deliberately NOT behind the governance entitlement the /usage feeds sit
// behind. That gate is about reading across teams; this read is scoped to a
// single conversation whose transcript and artifacts the caller can already see, and the
// actions are the part of that conversation's record which has no artifact to appear as
// — a review-thread reply, a refused merge, a denied push. Withholding them
// here would leave the incomplete answer the conversation view has today.
//
// POST /api/agent/conversations/{conversationID}/actions/list
func (ag *agentHandler) handleAgentActions(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}

	var req conversationActionListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		conv    *domain.Conversation
		actions []domain.ExternalAction
		total   int
	)
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		actions, total, e = tx.ExternalActions.ListByConversation(r.Context(), orgID, conversationID,
			domain.ExternalActionListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	out := make([]actionJSON, len(actions))
	for i, a := range actions {
		out[i] = toActionJSON(a)
	}
	httpx.WriteList(w, page, out, total)
}

// conversationResponse projects a Conversation into the wire shape the frontend consumes,
// augmented with `artifact_count` (so the Board card can show how many artifacts
// a conversation produced without a per-card fetch) and the derived approval signal:
// `has_unresolved_artifacts` (bool), `unresolved_pr_count` /
// `unresolved_review_count`, and `pending_artifact_ids` (the set of unresolved
// approvable artifact ids — all draft PRs + all ready reviews — for the per-item
// resolve UI). These replace the legacy `pending_kind` /
// `pending_artifact_id` overlay discriminators — approval is no longer a stored
// conversation status but a view over the unresolved-artifact set.
//
// Pure projection — it does no I/O. The caller supplies every input, which is
// what lets the conversation-list path batch its reads instead of issuing them
// per conversation:
//   - artifactCount from Artifacts.CountByConversation (the single-conversation path counts one
//     conversation; the list path batches every conversation in one query — N+1 avoidance).
//   - arts is the conversation's artifact set, which the caller reads best-effort (only
//     for conversations that have any artifact, so a conversation with none costs no list).
//   - stepCount is the length of the owning blueprint run's frozen plan, from
//     Blueprints.StepPlanLengths (batched the same way). 0 when the conversation has no
//     blueprint, and when the caller could not resolve one — a manual blueprint
//     run belongs to its creator under RLS, so a teammate reads 0 here and gets
//     the unqualified projection, which is what they already get for the chain
//     rail.
//
// stepCount is what makes a step's position legible: paired with
// blueprint_step_index it says whether a completed step is its chain's last —
// the difference between the task being over and the next step picking it up.
// Outcome alone cannot say that. It is the vocabulary an agent emits about its
// OWN ending, and what a given value implies for the chain depends on the
// runtime: under the SDK a terminal step reports `finish`, while the native
// loop stamps `continue` on every ordinary completion, final and single-step
// conversations included (internal/agentloop, internal/delegate/blueprint.go's
// blueprintDecisionForStepConversation is what resolves it against position).
//
// The derived approval keys are emitted only when the answer is definitive: when
// the conversation has no artifacts (artifactCount == 0, so nothing can be unresolved) or
// when the artifact set is actually in hand (len(arts) > 0). When artifactCount
// is positive but arts is empty — a best-effort read failed — the keys are
// OMITTED rather than reported as a misleading false, so a transient DB/RLS hiccup
// can't hide approval-required work; the client treats their absence as "unknown"
// and re-derives on the next refresh.
//
// currentAction is the derived prose line for a conversation that is actually
// working — composed by the caller from the newest assistant message's tool
// calls, "" when there is nothing honest to say (see current_action.go). It is
// emitted only when non-empty, and the SINGLE-conversation read passes "" on
// purpose: that route's client is the run station, whose live transcript
// already shows the call this line would summarize, and buying it there would
// add a query to a polled route for a field it does not render.
func conversationResponse(conv *domain.Conversation, artifactCount int, arts []domain.Artifact, stepCount int, currentAction string) map[string]any {
	out := map[string]any{
		"ID":            conv.ID,
		"TaskID":        conv.TaskID,
		"PromptID":      conv.PromptID,
		"Status":        conv.Status,
		"Model":         conv.Model,
		"StartedAt":     conv.StartedAt,
		"QueuedAt":      conv.QueuedAt,
		"ClaimedAt":     conv.ClaimedAt,
		"CompletedAt":   conv.CompletedAt,
		"TotalCostUSD":  conv.TotalCostUSD,
		"DurationMs":    conv.DurationMs,
		"NumTurns":      conv.NumTurns,
		"ParkReason":    string(conv.ParkReason),
		"WorktreePath":  conv.WorktreePath,
		"ResultSummary": conv.ResultSummary,
		// The terminal envelope's parsed outcome and its abort note. PascalCase
		// with the legacy set they belong to; empty string when the conversation holds
		// none (still executing, an infra failure, or an outcome gate that
		// exhausted its retries).
		"Outcome":              conv.Outcome,
		"OutcomeReason":        conv.OutcomeReason,
		"FailureKind":          string(conv.FailureKind),
		"SessionID":            conv.SessionID,
		"MemoryMissing":        conv.MemoryMissing,
		"TriggerType":          conv.TriggerType,
		"TriggerID":            conv.TriggerID,
		"actor_agent_id":       conv.ActorAgentID,
		"actor_agent_name":     conv.ActorAgentName,
		"blueprint_run_id":     conv.BlueprintRunID,
		"blueprint_step_index": conv.BlueprintStepIndex,
		"blueprint_step_count": stepCount,
		"artifact_count":       artifactCount,
		// The token rollups the conversation read already SUMs, alongside the cost /
		// duration / turns ones above. snake_case like every key added since
		// the legacy PascalCase set froze. Plain ints — 0 for a conversation that never
		// streamed a usage-bearing message — so a consumer never has to
		// distinguish absent from none.
		"input_tokens":          conv.InputTokens,
		"output_tokens":         conv.OutputTokens,
		"cache_read_tokens":     conv.CacheReadTokens,
		"cache_creation_tokens": conv.CacheCreationTokens,
	}
	// What a WORKING conversation is doing right now, in place of the title it
	// does not have. Composed by the caller, and emitted only when it had
	// something honest to say — an absent key means the client renders the
	// state label, never a fabricated action.
	if currentAction != "" {
		out["current_action"] = currentAction
	}
	// The queued run's place in its own org's line, emitted only when the
	// conversation actually has one — a running, open or terminal
	// conversation carries no key at all, never a 0 or a null, so a client
	// renders the mark on presence rather than on a sentinel. Org-local by
	// contract: it carries no fleet occupancy, no executor identity, no
	// placement tier and no cross-org term, and must not grow one. See
	// domain.Conversation.QueuePosition for why a deployment-truthful
	// position would be a cross-tenant disclosure.
	if conv.QueuePosition != nil {
		out["queue_position"] = *conv.QueuePosition
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

// transcriptPageSize bounds one GET …/messages read. It is the route's
// declared page size, not a clamp of a caller-supplied one: the transcript
// takes no page_size, because it is a sync/tail read whose page is always
// "as much recent history as is worth one request".
//
// 500 rows is far past any transcript a person scrolls in one sitting and far
// short of a conversation that streamed for hours.
const transcriptPageSize = 500

// transcriptResponse is the transcript's wire shape: the rows plus the token
// for the page BEFORE them.
//
// It is a GET, and deliberately not a POST /list. The transcript is followed,
// not browsed: a client holds a watermark and asks for what came after it,
// which is a cache-revalidating read of an append-only stream, and turning it
// into a body-carrying POST would buy nothing but a second spelling. It does
// take the list envelope's two paging keys so a client's loop looks the same.
//
// The two directions are separate on purpose:
//
//   - since_id walks FORWARD from a watermark the client already holds. It is
//     the tail-follow, used to repair a transcript assembled from websocket
//     frames.
//   - next_page_token walks BACKWARD through history from the oldest row this
//     response carries. It is how a client that opened on the tail reaches the
//     beginning.
//
// A response never carries a total: counting a stream that is still being
// appended to would be a number that is wrong by the time it renders.
type transcriptResponse struct {
	Items         []domain.MessageDTO `json:"items"`
	NextPageToken string              `json:"next_page_token,omitempty"`
}

func (ag *agentHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}
	// since_id is an optional watermark: the caller already holds every row up
	// to it and wants only what came after. A client repairing a transcript it
	// built from websocket frames polls with it.
	//
	// Absent means no watermark — the newest page. Anything else must be a
	// usable watermark: an unparseable or negative value is a 400 rather than a
	// silent fall-back, because "give me what I'm missing" answered with
	// something else is the widening a corrupt param must never buy. A negative
	// one would also reach the store as `id > -N`, which selects the whole
	// transcript today only because ids start at 1 — a coincidence, not a
	// contract. Surrounding whitespace is trimmed first (the convention for
	// query params here).
	var v httpx.Validation
	sinceID := nonNegativeIntParam(&v, r, "since_id")
	// page_token addresses older history. It is the opaque token the previous
	// response minted; a client never constructs one.
	beforeID := nonNegativeIntParam(&v, r, "page_token")
	if sinceID > 0 && beforeID > 0 {
		v.Param("page_token", "page_token walks history backward and since_id follows the tail; send one, not both")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var messages []domain.Message
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		messages, e = tx.Conversations.MessagesWindow(r.Context(), orgID, conversationID, db.MessageWindow{
			SinceID: sinceID, BeforeID: beforeID, Limit: transcriptPageSize,
		})
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}

	resp := transcriptResponse{Items: domain.MessageDTOs(messages)}
	if resp.Items == nil {
		resp.Items = []domain.MessageDTO{}
	}
	// A backward page exists only when this one is full AND we are not
	// tail-following: a short page reached the beginning, and a caller walking
	// forward from a watermark is not walking history at all. The token is the
	// oldest row's id — the ceiling the next page reads below.
	if sinceID == 0 && len(messages) == transcriptPageSize {
		resp.NextPageToken = strconv.Itoa(messages[0].ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// nonNegativeIntParam reads an optional non-negative integer query param,
// recording a fault on v rather than falling back to the default — a corrupt
// paging param must never widen the read it bounds. Absent yields 0.
func nonNegativeIntParam(v *httpx.Validation, r *http.Request, name string) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		v.Param(name, name+" must be a non-negative integer")
		return 0
	}
	return n
}

// handleAgentStop is the conversation-level stop verb: the agent stops, the
// conversation parks `open`, and everything outside it freezes — the blueprint
// keeps its status and the task keeps its disposition, which is what leaves the
// parked conversation resumable. Cancelling a plan is the blueprint endpoint's
// job; dispositioning work is the task gestures'.
//
// It is the single address for the gesture. The former /cancel and /interrupt
// are gone rather than aliased: two endpoints is how one gesture grew two
// meanings of `open` that a user could only tell apart by which button they
// pressed.
//
// A conversation not visible to the caller's org is 404 and discloses
// nothing — the visibility read is RLS-scoped, so an id from another tenant
// is indistinguishable from one that never existed. A conversation the caller
// CAN see that has already concluded is 409 ALREADY_TERMINAL, the same answer
// its sibling /message gives for the same state: the two verbs used to
// disagree (404 here, 409 there) about one condition on one resource.
// Anything else Stop returns is an internal fault on the way to stopping a conversation
// that really was active — a failed read, a failed park — and goes through
// internalError, which logs it and redacts the detail in multi mode. Reporting
// those as 404 would tell the user their conversation is gone when it is still running.
func (ag *agentHandler) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}
	spawner := ag.spawner()
	if spawner == nil {
		writeDelegationUnavailable(w)
		return
	}
	visible, err := ag.conversationVisible(r.Context(), orgID, userID, conversationID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !visible {
		notFound(w, "conversation")
		return
	}
	if err := spawner.Stop(orgID, conversationID, userID); err != nil {
		if errors.Is(err, delegate.ErrNoActiveConversation) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonAlreadyTerminal,
				Message: "this conversation has already concluded; there is nothing to stop",
			})
			return
		}
		internalError(w, "agent", err)
		return
	}
	ag.writeConversationResource(w, r, orgID, userID, conversationID)
}

// conversationVisible reports whether conversationID exists and is visible to the caller's org
// (and team, under RLS). The steering control ops resolve a conversation by id against
// in-memory state (the process registry, the permission broker), so this is the
// authorization gate that stops a known conversation id from one tenant being acted on by
// another: a non-existent or not-visible conversation reads as false → the caller 404s.
func (ag *agentHandler) conversationVisible(ctx context.Context, orgID, userID, conversationID string) (bool, error) {
	var exists bool
	err := ag.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		conv, e := tx.Conversations.Get(ctx, orgID, conversationID)
		if e != nil {
			return e
		}
		exists = conv != nil
		return nil
	})
	return exists, err
}

// handleMessage routes a free-form user message to a conversation: a live conversation is
// steered in place, a parked conversation is woken via resume. The conversation is read under
// the caller's org first, so a conversation not visible to this org is 404 — the authz
// gate before any control op.
//
// Recording belongs to the delegate layer, and it happens only when the
// message actually goes somewhere: a live steer records and broadcasts the
// row after the process takes the text, a follow-up queues it as the
// undelivered row a claim will drain. A conversation that can take no message
// (terminal / no live process) is 409 with nothing recorded and nothing
// broadcast — an optimistic write here used to leave a refused send painted
// onto every watcher's transcript as a message the agent never received,
// while the sender simultaneously saw an error toast.
func (ag *agentHandler) handleMessage(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}

	var body struct {
		Text string `json:"text"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "text is required", Field: "text",
		})
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		writeDelegationUnavailable(w)
		return
	}

	visible, err := ag.conversationVisible(r.Context(), orgID, userID, conversationID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !visible {
		notFound(w, "conversation")
		return
	}

	if err := spawner.SendMessage(r.Context(), orgID, conversationID, userID, body.Text); err != nil {
		// A conversation deleted between the visibility read above and the routing read
		// is a 404 like any other absent conversation — same body as the gate's own 404,
		// not a 409 that would send the client re-reading state that no longer
		// exists. Mirrors handleAgentStop's ErrNoActiveConversation mapping.
		if errors.Is(err, delegate.ErrConversationNotFound) {
			notFound(w, "conversation")
			return
		}
		status, reason := steerErrorStatus(err)
		if status == http.StatusInternalServerError {
			// Never the raw text: a steer failure at this rung is a server
			// fault whose error may carry driver/process detail, so it takes
			// internalError's log-and-redact path like any other 500.
			internalError(w, "agent", err)
			return
		}
		// The 4xx arms are all delegate sentinels — author-written state
		// descriptions with nothing internal in them, and the text is the
		// answer ("this conversation has moved on"), so it is surfaced.
		httpx.WriteErrors(w, status, httpx.ErrorItem{Reason: reason, Message: err.Error()})
		return
	}
	ag.writeConversationResource(w, r, orgID, userID, conversationID)
}

// steerErrorStatus maps a SendMessage error to an HTTP status. A
// conversation that can't take the op right now — no live process (ErrNoLiveProcess),
// not steerable (ErrConversationNotSteerable), or moved out of a resumable state
// (ErrConversationNotResumable) — is 409 Conflict so the client refreshes and re-reads
// the conversation's state. Two wakes racing is NOT among them: the loser's message is
// queued alongside the winner's and delivered by the winner's claim, so it
// returns nil and the client is told "sent", which is what happened. A wake
// that loses to a conversation going terminal still 409s — nothing will claim
// it, so nothing delivers the message. An expired workspace (ErrWorkspaceExpired) is 410 Gone: the
// conversation's saved state was reaped after the retention window, so retrying won't
// help — the client surfaces the clear error rather than a transient conflict.
// A model the conversation's team may no longer pick (domain.ErrModelNotEnabled)
// is 409 for the same reason as its neighbours and NOT the 422 a refused
// delegation answers with: this is a rung of the resume ladder, so the composer
// the run read disabled and the send refused here have to agree, and the client
// re-reads the conversation to find out what changed. It is also the one rung a
// person clears without touching the conversation at all — the message names
// the model and the set, and re-enabling makes the very next send succeed.
//
// A concluded conversation (ErrConversationConcluded) is 409 as well, but it
// carries its own message: this conversation's blueprint will never drive it
// again (it has moved past this step). A cancelled one (ErrBlueprintCancelled)
// is the same class — the blueprint was called off at its own layer — under
// its own words. Both are permanent answers rather than a lost race the client
// should re-read — surface the text, don't prompt a refresh. A step that just
// handed off (ErrStepHandedOff) is 409 for the opposite reason: the
// conversation concluded and its blueprint has not reacted yet, so the answer
// genuinely does change a beat later and a refresh is exactly what resolves it.
// A cross-pod signal whose owning executor never acked (ErrSignalAckTimeout,
// TFAC-585) is 504 Gateway Timeout — the reply-leg contract's "conversation owner did
// not acknowledge; the conversation may be mid-teardown" case; the UI already
// tolerates a failed steer. Everything else is a server-side 500.
//
// The reason travels with the status because two of these conflicts are not
// the same class: a concluded conversation is ALREADY_TERMINAL (permanent,
// matching what /stop answers for the same state), while a lost race or a
// parked process is a plain CONFLICT the client resolves by re-reading.
func steerErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, delegate.ErrWorkspaceExpired):
		return http.StatusGone, httpx.ReasonConflict
	case errors.Is(err, delegate.ErrSignalAckTimeout):
		return http.StatusGatewayTimeout, httpx.ReasonUpstreamUnavailable
	case errors.Is(err, delegate.ErrConversationConcluded),
		errors.Is(err, delegate.ErrBlueprintCancelled):
		return http.StatusConflict, httpx.ReasonAlreadyTerminal
	case errors.Is(err, delegate.ErrNoLiveProcess),
		errors.Is(err, delegate.ErrConversationNotSteerable),
		errors.Is(err, delegate.ErrConversationNotResumable),
		errors.Is(err, delegate.ErrStepHandedOff),
		errors.Is(err, domain.ErrModelNotEnabled):
		return http.StatusConflict, httpx.ReasonConflict
	default:
		return http.StatusInternalServerError, httpx.ReasonInternal
	}
}

// handleAgentPermission answers a pending tool-permission prompt a conversation surfaced
// via a `permission_request` WS event. Body: {"behavior":"allow"|"deny",
// "message"?:string,"updated_input"?:object}. The path's tool call id is the
// tool_use id the prompt was raised for. The conversation is authorized under the
// caller's org (RLS) first — like the message/interrupt endpoints — so a conversation not
// visible to this org is 404; a prompt that isn't pending (already answered,
// timed out, or never existed) is also 404. A 200 always answers with
// agentPermissionResolvedResponse — see its doc for why `permission` is
// sometimes absent.
func (ag *agentHandler) handleAgentPermission(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}
	toolCallID := r.PathValue("toolCallID")

	var body struct {
		Behavior     string         `json:"behavior"`
		Message      string         `json:"message"`
		UpdatedInput map[string]any `json:"updated_input"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: `behavior must be "allow" or "deny"`, Field: "behavior",
		})
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		writeDelegationUnavailable(w)
		return
	}

	// Authorize the conversation under the caller's org before touching the broker — the
	// broker's own org check is a backstop, not a team-level RLS gate.
	exists, err := ag.conversationVisible(r.Context(), orgID, userID, conversationID)
	if err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "conversation")
		return
	}

	resolved, err := spawner.ResolvePermission(orgID, conversationID, toolCallID, userID, agentproc.PermissionDecision{
		Behavior:     body.Behavior,
		Message:      body.Message,
		UpdatedInput: body.UpdatedInput,
	})
	switch {
	case errors.Is(err, delegate.ErrNoPendingPermission):
		notFound(w, "permission request")
		return
	case errors.Is(err, delegate.ErrSignalAckTimeout):
		httpx.WriteErrors(w, http.StatusGatewayTimeout, httpx.ErrorItem{
			Reason: httpx.ReasonUpstreamUnavailable, Message: err.Error(),
		})
		return
	case err != nil:
		internalError(w, "agent", err)
		return
	}

	// The broker delivered the decision either way (err == nil). resolved is
	// the row when this process settled it locally; it's nil on two other
	// paths that are NOT errors: the cross-pod routed case (the write
	// happened on the remote owner, not here) and the best-effort-record
	// failure case (Create/Resolve never produced a row at all — see their
	// docs; a missing claim is one way there). The first is recoverable by
	// reading the row back — a genuine read of state a DIFFERENT process
	// just wrote, not the re-read-your-own-write pattern the returned-row
	// standard exists to remove, and done under the caller's own claims
	// (RLS), matching handleAgentPermissions below. The second genuinely has
	// no row anywhere; failing to record a prompt must never become a
	// reason to fail to answer it, so that stays a 200 with nothing to show.
	if resolved == nil {
		if txErr := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var getErr error
			resolved, getErr = tx.Permissions.Get(r.Context(), orgID, conversationID, toolCallID)
			return getErr
		}); txErr != nil {
			internalError(w, "agent", txErr)
			return
		}
	}
	// One fixed response shape regardless of path: `permission` is present
	// when a row was found (locally, or via the cross-pod fallback read) and
	// omitted — never a differently-typed stub — when there's genuinely
	// nothing to show. A client can always read `status` unconditionally.
	writeJSON(w, http.StatusOK, agentPermissionResolvedResponse{Status: "resolved", Permission: resolved})
}

// agentPermissionResolvedResponse is handleAgentPermission's one 200 body
// shape. Permission is nil exactly when there's no audit row to answer with
// (see handleAgentPermission's doc) — never represented by swapping in a
// differently-shaped body, so a client can always safely read Status without
// a type switch.
type agentPermissionResolvedResponse struct {
	Status     string                         `json:"status"`
	Permission *domain.ConversationPermission `json:"permission,omitempty"`
}

// handleAgentPermissions lists the tool-approval prompts a conversation is
// currently waiting on. This is the address a pending prompt didn't have: the
// `permission_request` websocket frame is fire-once, so a refresh, a second
// tab, or a cold board load could never learn that a healthy, parked agent was
// waiting on a human — the prompt just sat until the server-side timeout denied
// it. The frame is now a hint that points here, matching every other event type.
//
// Pending only. A history read is for the audit UI, which doesn't exist yet;
// an ?include=all now would be a filter with no caller.
//
// Authorized exactly like handleAgentPermission: conversationVisible under the caller's
// org before touching anything, so a conversation this org can't see is 404
// rather than an empty list (which would confirm the id exists).
func (ag *agentHandler) handleAgentPermissions(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}

	var pending []domain.ConversationPermission
	var exists bool
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		conv, e := tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		exists = true
		pending, e = tx.Permissions.ListPending(r.Context(), orgID, conversationID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if !exists {
		notFound(w, "conversation")
		return
	}
	writeJSON(w, http.StatusOK, domain.PendingPermissionDTOs(pending, time.Now().UTC()))
}

// enrichConversations projects conversations onto the wire shape, augmenting each with a batched
// artifact_count and the derived has_unresolved_artifacts (+ per-kind counts).
// Both artifact reads stay O(1) in queries regardless of how many conversations the set
// has. Board's useWebSocket re-fetches on every status transition, so a per-conversation
// read would scale with the task's conversation history.
//
//   - artifact_count for every conversation: one CountByConversation (conversationID→count; a conversation with no
//     artifacts is absent → 0).
//   - has_unresolved_artifacts for the conversations that have artifacts: one ListByConversations
//     over just those conversation ids (count>0), grouped per conversation. The derivation needs
//     the actual artifacts, but a conversation with none can't have an unresolved one, so
//     it costs no list.
//   - blueprint_step_count for the conversations that belong to a blueprint: one
//     StepPlanLengths over the deduped blueprint_run ids.
//   - current_action for the conversations displaying as `running`: one
//     NewestAssistantToolCallsForConversations over just those ids, composed
//     into prose per current_action.go. Only running conversations are read —
//     every other state already has an honest label of its own, and the newest
//     tool call on a stopped conversation describes something that is no longer
//     happening.
//
// Deliberately best-effort: a ListByConversations, StepPlanLengths or
// NewestAssistantToolCallsForConversations failure drops what that read
// contributes (logged) but leaves counts and the rest intact. These
// are display annotations on rows the caller can already see, so surfacing the
// failure would fail a whole board refresh over a badge. Shared by the
// single-task and batched (task_ids) conversation-list paths.
func enrichConversations(ctx context.Context, tx db.TxStores, orgID string, convs []domain.Conversation) ([]map[string]any, error) {
	conversationIDs := make([]string, len(convs))
	for i := range convs {
		conversationIDs[i] = convs[i].ID
	}
	counts, err := tx.Artifacts.CountByConversation(ctx, orgID, conversationIDs)
	if err != nil {
		return nil, err
	}
	var withArtifacts []string
	for i := range convs {
		if counts[convs[i].ID] > 0 {
			withArtifacts = append(withArtifacts, convs[i].ID)
		}
	}
	artsByConv := map[string][]domain.Artifact{}
	if len(withArtifacts) > 0 {
		if arts, lerr := tx.Artifacts.ListByConversations(ctx, orgID, withArtifacts); lerr != nil {
			serverLog.Warn("artifact batch lookup failed; omitting has_unresolved_artifacts (artifact_count unaffected)", "error", lerr)
		} else {
			for _, a := range arts {
				artsByConv[a.ConversationID] = append(artsByConv[a.ConversationID], a)
			}
		}
	}
	// One read for every blueprint in the set, keyed by blueprint_run id and
	// deduped first (a chain's steps all name the same one). Best-effort: a
	// failure leaves every blueprint_step_count at 0, which reads as "position
	// unknown" and costs the qualifier, not the row.
	var blueprintRunIDs []string
	seenBlueprints := map[string]bool{}
	for i := range convs {
		id := convs[i].BlueprintRunID
		if id == "" || seenBlueprints[id] {
			continue
		}
		seenBlueprints[id] = true
		blueprintRunIDs = append(blueprintRunIDs, id)
	}
	stepCounts := map[string]int{}
	if len(blueprintRunIDs) > 0 {
		if lens, lerr := tx.Blueprints.StepPlanLengths(ctx, orgID, blueprintRunIDs); lerr != nil {
			serverLog.Warn("blueprint step-plan length batch lookup failed; omitting blueprint_step_count", "error", lerr)
		} else {
			stepCounts = lens
		}
	}
	// current_action for the conversations that are actually working, and only
	// those: a queued conversation has nobody driving it, a parked or terminal
	// one has stopped, and the newest tool call on any of them describes
	// something that is no longer happening. Their display status already says
	// what they are (see internal/domain/conversation_status.go); `running` is
	// the one that says nothing.
	var running []string
	for i := range convs {
		if convs[i].Status == domain.StatusRunning {
			running = append(running, convs[i].ID)
		}
	}
	toolCalls := map[string][]domain.ToolCall{}
	if len(running) > 0 {
		if calls, lerr := tx.Conversations.NewestAssistantToolCallsForConversations(ctx, orgID, running); lerr != nil {
			serverLog.Warn("newest tool-call batch lookup failed; omitting current_action", "error", lerr)
		} else {
			toolCalls = calls
		}
	}
	out := make([]map[string]any, len(convs))
	for i := range convs {
		out[i] = conversationResponse(&convs[i], counts[convs[i].ID], artsByConv[convs[i].ID],
			stepCounts[convs[i].BlueprintRunID], currentAction(toolCalls[convs[i].ID]))
	}
	return out, nil
}

// maxBatchTaskIDs caps how many task ids one conversation-list request
// names, bounding the DB work and response size a single call can trigger.
// 500 matches the store's IN-list chunk size, so the id set is always one
// chunk; a larger set is rejected rather than truncated. It caps the id set
// only — a request naming no task is bounded by its page like any other list.
const maxBatchTaskIDs = 500

// maxBatchTeamIDs caps how many team ids one conversation-list request names.
// A team set is a view scope — the Overview sends one, a whole-org union sends
// as many teams as the viewer belongs to — so 100 is generous for the question
// and still leaves the SQLite statement inside the 999-variable budget the
// oldest supported build enforces: a full 500-id task chunk plus a team list
// this size plus the status placeholders. Rejected rather than truncated, like
// the task cap above, because a silently dropped team is a count that reads as
// real work disappearing.
const maxBatchTeamIDs = 100

// conversationListRequest is the body of POST /api/agent/conversations/list.
//
// Every field is optional — `{}` is "every conversation I can see, first
// page" — and each one narrows: naming no task is no task filter, not an empty
// selection, which is what lets one route answer both the Board's per-task
// batch and a resource-wide count. The response shape does not vary with them:
// rows keyed by task id, which is what every caller (the board, the task
// drawer) groups by, and which a count-only read answers as an empty map.
type conversationListRequest struct {
	// TaskIDs selects the tasks whose conversations to return. Optional: absent
	// or empty is no task narrowing — the viewer's whole visible set under RLS,
	// which is what a resource-wide question ("how many are running") has to be
	// able to ask. When present, at most maxBatchTaskIDs, each a valid task id.
	TaskIDs []string `json:"task_ids"`
	// TeamIDs narrows to the conversations these teams own. Optional: absent
	// or empty is no team narrowing — the viewer-wide read the shell rail
	// asks for. A present set that is unusable is a 400, never a silent
	// widening back to every team; the Overview's team-scoped counts would
	// otherwise report another team's work as this team's. When present, at
	// most maxBatchTeamIDs, each a valid team id.
	TeamIDs []string `json:"team_ids"`
	// Statuses narrows to these DISPLAY statuses — the value every surface
	// shows, which is why a claim phase is a legal value here and the stored
	// column is not what gets filtered. Empty = every status. An unknown value
	// is a client fault, not an empty page.
	Statuses []string `json:"statuses"`
	// Attention keeps only the conversations waiting on a human: an unanswered
	// permission prompt, or a not-live conversation still holding an unresolved
	// artifact. False narrows nothing.
	Attention bool `json:"attention"`
	// IncludeMessages adds each task's PRIMARY (newest-started) conversation's
	// transcript, keyed by that conversation's id — what the Board seeds onto a card.
	IncludeMessages bool `json:"include_messages"`

	httpx.PageRequest
}

type conversationListFilterKey struct {
	TaskIDs         []string `json:"task_ids"`
	TeamIDs         []string `json:"team_ids"`
	Statuses        []string `json:"statuses"`
	Attention       bool     `json:"attention"`
	IncludeMessages bool     `json:"include_messages"`
}

// conversationListResponse is the list envelope with the conversation rows grouped by
// task id rather than as a flat `items` array.
//
// The grouping is the shape callers actually consume, and it is why this route
// spells its own envelope instead of using httpx.WriteList: a map cannot be a
// slice. The paging keys mean exactly what they mean everywhere else — the
// window is over the CONVERSATIONS, ordered (task_id, started_at DESC, id), so
// a task's conversations stay contiguous and total_count counts conversations, not
// tasks.
//
// Runs carry the frozen run projection byte-for-byte, PascalCase legacy keys
// and all. Renaming them is its own change, not a side effect of this one.
type conversationListResponse struct {
	Runs          map[string][]map[string]any    `json:"runs"`
	Messages      map[string][]domain.MessageDTO `json:"messages,omitempty"`
	NextPageToken string                         `json:"next_page_token,omitempty"`
	TotalCount    int                            `json:"total_count"`
}

// handleConversations serves POST /api/agent/conversations/list.
//
// Reading every task in one WithTx makes the snapshot internally consistent:
// a per-task serial loop read each task at a different transaction boundary,
// so a status change mid-refresh could return some tasks in the old state and
// some in the new. One tx removes that flicker class.
//
// Two shapes of caller, one read. The Board names its tasks and consumes the
// grouped envelope. The shell's live rail names none and asks the resource
// what it holds — "how many are running", "how many need me" — as a count-only
// page (page_size: 0), which is what a count of rows is under the list
// contract. Its answer is the viewer's own visible set, because the filters
// narrow within RLS rather than around it.
func (ag *agentHandler) handleConversations(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req conversationListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	taskIDs := canonicalStrings(req.TaskIDs)
	if len(taskIDs) > maxBatchTaskIDs {
		v.OutOfRange("task_ids", fmt.Sprintf("task_ids accepts at most %d ids per request; got %d", maxBatchTaskIDs, len(taskIDs)))
	}
	for _, id := range taskIDs {
		// tasks.id is a uuid column on Postgres, so a malformed selector would
		// otherwise reach the store as a 22P02 → 500.
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("task_ids", fmt.Sprintf("task_ids contains %q, which is not a valid task id", id))
		}
	}
	teamIDs := canonicalStrings(req.TeamIDs)
	// Counted after canonicalization, so a client that repeated a team id
	// spends the cap on distinct teams rather than on its own duplicates.
	if len(teamIDs) > maxBatchTeamIDs {
		v.OutOfRange("team_ids", fmt.Sprintf("team_ids accepts at most %d ids per request; got %d", maxBatchTeamIDs, len(teamIDs)))
	}
	for _, id := range teamIDs {
		// conversations.team_id is a uuid column on Postgres, the same reason
		// the task selector above parses rather than trusting.
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("team_ids", fmt.Sprintf("team id %q is not a valid team id", id))
		}
	}
	statuses := canonicalStrings(req.Statuses)
	validStatuses := domain.AllConversationStatuses()
	for _, st := range statuses {
		if !slices.Contains(validStatuses, st) {
			v.Invalid("statuses", fmt.Sprintf("unknown status %q; must be one of: %s",
				st, strings.Join(validStatuses, ", ")))
		}
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(conversationListFilterKey{
		TaskIDs: taskIDs, TeamIDs: teamIDs, Statuses: statuses,
		Attention: req.Attention, IncludeMessages: req.IncludeMessages,
	}), maxBatchTaskIDs)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	resp := conversationListResponse{Runs: map[string][]map[string]any{}}
	if req.IncludeMessages {
		resp.Messages = map[string][]domain.MessageDTO{}
	}
	if err := ag.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		convs, total, e := tx.Conversations.List(r.Context(), orgID,
			db.ConversationListFilter{TaskIDs: taskIDs, TeamIDs: teamIDs, Statuses: statuses, Attention: req.Attention},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		if e != nil {
			return e
		}
		resp.TotalCount = total
		// One enrichConversations over the whole page keeps the artifact reads O(1)
		// regardless of task/conversation count; the index alignment lets us group the
		// projected responses by task without re-projecting.
		enriched, e := enrichConversations(r.Context(), tx, orgID, convs)
		if e != nil {
			return e
		}
		var primaryConversationIDs []string
		seenTask := map[string]bool{}
		for i := range convs {
			tid := convs[i].TaskID
			resp.Runs[tid] = append(resp.Runs[tid], enriched[i])
			// Conversations come back started_at DESC within a task, so the first seen
			// per task is its primary (newest) conversation — the one whose transcript
			// the card shows. "First seen in this page": a task whose conversations
			// began on an earlier page has no primary here, which is the same
			// thing as having no rows here.
			if !seenTask[tid] {
				seenTask[tid] = true
				primaryConversationIDs = append(primaryConversationIDs, convs[i].ID)
			}
		}
		if req.IncludeMessages && len(primaryConversationIDs) > 0 {
			msgs, e := tx.Conversations.MessagesForConversations(r.Context(), orgID, primaryConversationIDs)
			if e != nil {
				return e
			}
			for _, m := range msgs {
				resp.Messages[m.ConversationID] = append(resp.Messages[m.ConversationID], m.ToDTO())
			}
		}
		return nil
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	resp.NextPageToken = httpx.NextPageToken(page, len(flattenConversationGroups(resp.Runs)), resp.TotalCount)
	writeJSON(w, http.StatusOK, resp)
}

// flattenConversationGroups counts the rows a grouped page carries, so the next-token
// arithmetic sees the same number a flat `items` array would have.
func flattenConversationGroups(groups map[string][]map[string]any) []map[string]any {
	var out []map[string]any
	for _, rows := range groups {
		out = append(out, rows...)
	}
	return out
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}
