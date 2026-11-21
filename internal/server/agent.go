package server

import (
	"context"
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
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/deliverables"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/models"
	"github.com/sky-ai-eng/triage-factory/internal/server/usage"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
	"go.opentelemetry.io/otel/trace"
)

// agentHandler serves the agent-conversation endpoints (status / messages / cancel /
// message / interrupt / permissions / conversation list / deliverable refresh). spawner and
// reconciler are read through getters so the handler always sees the current
// instance, (re)wired onto the server after construction.
type agentHandler struct {
	tx         db.TxRunner
	az         *authz.Checker
	ws         *websocket.Hub
	spawner    func() *delegate.Spawner
	reconciler func() *reconcile.Reconciler
}

// conversationIDOr404 guards the {conversationID} path value — conversations.id
// is a uuid column on Postgres. See uuidPathOr404.
func conversationIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	return uuidPathOr404(w, r, "conversationID", "conversation")
}

// traceConversation puts conversationID on the request's server span, so a
// search on the id finds the request beside the engagement traces of the
// conversation it changed. Only the write routes call it: a client watching a
// conversation polls the reads, and those traces would crowd out the ones a
// search on the id is for.
func traceConversation(r *http.Request, conversationID string) {
	trace.SpanFromContext(r.Context()).SetAttributes(telemetry.ConversationID(conversationID))
}

var opConversationGet = httpx.NewJSONOp[httpx.NoBody, conversationJSON](httpx.OpSpec{
	Pattern:     "GET /api/agent/conversations/{conversationID}",
	ID:          "get_conversation",
	Summary:     "Get a conversation",
	Description: "Returns one conversation the caller can see: its status, spend, blueprint step, the deliverables waiting on a person, whether a message sent now would continue it, the settings such a message would run on, and whether the caller may act on it.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	ag.writeConversationResource(w, r, opConversationGet.Write, orgID, userID, conversationID)
}

// writeConversationResource reads the conversation resource exactly as
// handleAgentStatus does — the point read plus its display enrichment
// (artifact counts, the derived approval signal, blueprint step count,
// resumability, the next message's settings, and the cache's warmth) — and
// writes it as the response through write, the answering route's op. Shared
// with handleAgentStop/handleMessage so a mutating verb route answers with the
// resource it just changed rather than a status stub naming only the field
// that verb happens to touch.
func (ag *agentHandler) writeConversationResource(w http.ResponseWriter, r *http.Request, write func(http.ResponseWriter, conversationJSON), orgID, userID, conversationID string) {
	var conv *domain.Conversation
	var resp conversationJSON
	var newestAssistant *domain.Message
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		counts, e := tx.Deliverables.CountByConversation(r.Context(), orgID, []string{conv.ID})
		if e != nil {
			return e
		}
		artifactCounts, e := tx.Artifacts.CountByConversations(r.Context(), orgID, []string{conv.ID})
		if e != nil {
			return e
		}
		actionCounts, e := tx.ExternalActions.CountByConversations(r.Context(), orgID, []string{conv.ID})
		if e != nil {
			return e
		}
		// Derive has_unresolved_deliverables (+ per-kind counts) from the conversation's
		// deliverable set. Only read the deliverables when the conversation has any —
		// counts==0 means there's nothing unresolved, so skip the list. A list
		// failure is best-effort BY DESIGN — this is display enrichment on top
		// of an authoritative row, so it omits the derived flags (logged) but
		// must not fail the status fetch or touch the count above. The
		// swallowed-error sweep leaves it swallowed deliberately.
		var arts []domain.Deliverable
		if counts[conv.ID] > 0 {
			if a, _, lerr := tx.Deliverables.ListByConversation(r.Context(), orgID, conv.ID, db.Unwindowed); lerr != nil {
				serverLog.Warn("deliverable lookup for has_unresolved_deliverables failed; omitting it", "conversation", conv.ID, "error", lerr)
			} else {
				arts = a
			}
		}
		// The owning blueprint's plan length and status, so the conversation
		// page can tell a handed-off step from the one that ended the task, and
		// whether the work is done. Best-effort like the deliverable list above: a
		// failure (or a blueprint row RLS hides) leaves both unresolved and the
		// projection unqualified, never a failed status fetch.
		var progress db.RunProgress
		if conv.BlueprintRunID != "" {
			if runs, lerr := tx.Blueprints.RunProgress(r.Context(), orgID, []string{conv.BlueprintRunID}); lerr != nil {
				serverLog.Warn("blueprint run progress lookup failed; omitting blueprint_step_count and blueprint_run_status", "conversation", conv.ID, "blueprint_run", conv.BlueprintRunID, "error", lerr)
			} else {
				progress = runs[conv.BlueprintRunID]
			}
		}
		resp = conversationResponse(conv, counts[conv.ID], arts, artifactCounts[conv.ID], actionCounts[conv.ID], progress, "")
		// Not best-effort like the reads above: on Postgres a failed
		// statement aborts the transaction, so its commit fails the read
		// either way, and a SQLite read answers the same.
		newestAssistant, e = tx.Conversations.NewestAssistant(r.Context(), orgID, conv.ID)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}
	addCacheWarmUntil(&resp, newestAssistant)
	addResumability(r.Context(), &resp, orgID, conv, ag.spawner())
	addNextMessageSettings(r.Context(), &resp, orgID, conv, ag.spawner())
	rows := []conversationJSON{resp}
	ag.addCanWrite(r.Context(), orgID, userID, []string{conv.ID}, rows)
	write(w, rows[0])
}

// addCacheWarmUntil puts on the detail read when the provider's prompt
// cache from the newest active assistant turn goes cold: that turn's time
// plus domain.ProviderCacheTTL, in RFC 3339 UTC, for the client to compare
// with the current time. Warmth is derived, never stored, and the field is
// absent for a conversation with no assistant turn in its window.
func addCacheWarmUntil(resp *conversationJSON, newestAssistant *domain.Message) {
	if newestAssistant == nil {
		return
	}
	resp.CacheWarmUntil = newestAssistant.CreatedAt.Add(domain.ProviderCacheTTL).UTC().Format(time.RFC3339)
}

// addNextMessageSettings puts on the detail read the settings a message that
// names none would run on (Spawner.NextMessageSettings), which the composer
// opens with: next_message_model, and next_message_effort when the effort is
// not the provider's default. Both are omitted when the send would refuse
// such a message, as it does on a team whose default model is no longer
// enabled or on a model whose provider the organization has not connected,
// and the composer then asks for a model; and, best-effort like the read's
// other enrichment, when a read fails.
func addNextMessageSettings(ctx context.Context, resp *conversationJSON, orgID string, conv *domain.Conversation, spawner *delegate.Spawner) {
	if spawner == nil || conv == nil {
		return
	}
	settings, err := spawner.NextMessageSettings(ctx, orgID, *conv)
	if err != nil {
		// An organization with no credentials is a refusal as well, one that
		// names no field.
		var refusal *delegate.SettingsFieldError
		if !errors.As(err, &refusal) && !errors.Is(err, modelaccess.ErrNoCredentials) {
			serverLog.Warn("next message settings lookup failed; omitting next_message_model and next_message_effort", "conversation", conv.ID, "error", err)
		}
		return
	}
	if settings.Model == "" {
		return
	}
	resp.NextMessageModel = settings.Model
	resp.NextMessageEffort = settings.Effort
}

// addCanWrite puts requireWritableConversation's answer on each conversation
// a read returns, as `can_write`, so a client offers the composer, Stop, and
// Allow/Deny only to a caller the routes would accept. It is the same probe
// the gate runs (authz.Checker.WritableConversations), batched over the rows,
// so the control a client renders and the answer the route gives cannot
// disagree; the routes still decide.
//
// ids and rows are index-aligned. Best-effort, like the read's other display
// enrichment: a failed probe omits the key on every row, which a client reads
// as unknown rather than as a refusal. It runs outside the read's transaction
// because the probe opens its own.
func (ag *agentHandler) addCanWrite(ctx context.Context, orgID, userID string, ids []string, rows []conversationJSON) {
	if len(ids) == 0 {
		return
	}
	writable, err := ag.az.WritableConversations(ctx, userID, orgID, ids)
	if err != nil {
		serverLog.Warn("conversation-write probe failed; omitting can_write", "error", err)
		return
	}
	for i, id := range ids {
		canWrite := writable[id]
		rows[i].CanWrite = &canWrite
	}
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
func addResumability(ctx context.Context, resp *conversationJSON, orgID string, conv *domain.Conversation, spawner *delegate.Spawner) {
	if spawner == nil || conv == nil {
		return
	}
	if domain.IsActiveConversationStatus(conv.Status) || conv.Status == domain.StatusFailed {
		return
	}
	ok, reason := spawner.ResumabilityFor(ctx, orgID, conv)
	resp.Resumable = &ok
	resp.ResumeBlockedReason = reason
}

var opConversationDeliverablesRefresh = httpx.NewJSONOp[httpx.NoBody, deliverableRefreshResponse](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/deliverables/refresh",
	ID:          "refresh_conversation_deliverables",
	Summary:     "Refresh a conversation's deliverables",
	Description: "Reads each of the conversation's draft and open pull requests from GitHub, stores the state GitHub reports, and returns how many changed state.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleDeliverableRefresh is the Tier-2, conversation-scoped half of deliverable
// reconciliation (TFAC-464): the conversation view polls it (~5s while open) to pull a
// conversation's non-terminal deliverables fresh against GitHub without waiting for the
// background per-org cycle. It runs the SAME reconcile path as Tier 1, bounded
// to this one conversation's deliverables, and pushes any transition over the WS hub.
//
// Authorization rides the request-claims read: Conversations.Get + Deliverables.ListByConversation
// are RLS-scoped, so a user can only refresh a conversation their team can see. The GitHub
// calls and the deliverable/memory writes happen OUTSIDE that read tx (no network
// I/O under a held transaction) — the reconciler writes via its admin-pool path.
func (ag *agentHandler) handleDeliverableRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}
	traceConversation(r, conversationID)

	rc := ag.reconciler()
	if rc == nil {
		// Same reading as a missing spawner: this deployment runs no
		// reconciler, so the route has nothing to drive.
		writeNotConfigured(w, "deliverable reconciliation is not configured on this deployment")
		return
	}

	// Read the conversation + its reconcilable non-terminal deliverables under request
	// claims (authorization + RLS scoping). Filter to the same working set the
	// org-wide Tier-1 query selects so the two tiers reconcile identically.
	var conv *domain.Conversation
	var arts []domain.Deliverable
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil || conv == nil {
			return e
		}
		all, _, e := tx.Deliverables.ListByConversation(r.Context(), orgID, conversationID, db.Unwindowed)
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
	opConversationDeliverablesRefresh.Write(w, deliverableRefreshResponse{Updated: len(updated)})
}

// deliverableRefreshResponse is the body of POST
// /api/agent/conversations/{conversationID}/deliverables/refresh.
type deliverableRefreshResponse struct {
	Updated int `json:"updated" doc:"How many of the conversation's pull requests changed state."`
}

// conversationDeliverableListRequest is the body of POST
// /api/agent/conversations/{conversationID}/deliverables/list. The
// conversation is the path id and the read has no filters of its own, so the
// body is paging alone.
type conversationDeliverableListRequest struct {
	httpx.PageRequest
}

var opConversationDeliverables = httpx.NewListOp[conversationDeliverableListRequest, deliverables.JSON](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/deliverables/list",
	ID:          "list_conversation_deliverables",
	Summary:     "List a conversation's deliverables",
	Description: "Returns one page of the pull requests and reviews the conversation prepared, newest first, each saying whether the caller may act on it.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleAgentDeliverables returns the deliverables a conversation prepared — its
// pull requests and reviews — newest first, one page at a time. It reads the
// conversation first under request claims, so a conversation the caller's team
// can't see is a 404 (RLS scopes both reads), never a leak; a member of the
// owning team gets the list. Backs the conversation detail surface; team-level
// aggregation is the usage feed, not here.
//
// POST /api/agent/conversations/{conversationID}/deliverables/list
func (ag *agentHandler) handleAgentDeliverables(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	conversationID, ok := conversationIDOr404(w, r)
	if !ok {
		return
	}

	req, ok := opConversationDeliverables.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		conv  *domain.Conversation
		arts  []domain.Deliverable
		total int
	)
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		arts, total, e = tx.Deliverables.ListByConversation(r.Context(), orgID, conversationID,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	out := make([]deliverables.JSON, len(arts))
	for i, a := range arts {
		out[i] = deliverables.ToJSON(a)
	}
	deliverables.AddCanWrite(r.Context(), ag.az, orgID, userID, arts, out)
	opConversationDeliverables.Write(w, page, out, total)
}

// conversationActionListRequest is the body of POST
// /api/agent/conversations/{conversationID}/actions/list. The conversation is
// the path id. ObjectID keeps the rows of one artifact (its object_id): the
// rows that changed that object and that its count counts, which leaves out
// TF's own; Refused keeps the refused and failed writes. Both
// are optional and may be given together. The filters are fields of the body
// itself rather than an embedded struct, so a type fault names them as the
// caller spelled them.
type conversationActionListRequest struct {
	ObjectID string `json:"object_id" doc:"The object_id of one of the conversation's artifacts, to return only the actions that changed that object; absent or empty applies no object filter."`
	Refused  bool   `json:"refused" doc:"Whether to return only the actions that were refused or failed."`

	httpx.PageRequest
}

var opConversationActions = httpx.NewListOp[conversationActionListRequest, usage.ActionJSON](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/actions/list",
	ID:          "list_conversation_actions",
	Summary:     "List a conversation's actions",
	Description: "Returns one page of the actions the conversation recorded on outside systems, newest first, including refused and failed writes and writes that produced no object, narrowed by the body's filters.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// conversationActionFilters is the canonical filter set the page token is
// fingerprinted over, and what the store reads.
type conversationActionFilters struct {
	ObjectID domain.ObjectRef `json:"object_id"`
	Refused  bool             `json:"refused"`
}

// handleAgentActions returns the external actions this conversation performed — the
// audit log of record, filtered to one conversation and newest first. The
// sibling of handleAgentDeliverables, and gated identically: the conversation is read first
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
// single conversation whose transcript and deliverables the caller can already see, and the
// actions are the part of that conversation's record which has no deliverable to appear as
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

	req, ok := opConversationActions.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	filters := conversationActionFilters{Refused: req.Refused}
	if req.ObjectID != "" {
		id, ok := domain.ParseObjectRef(req.ObjectID)
		if !ok {
			v.Invalid("object_id", "must be an object id, as an artifact's object_id spells it")
		}
		filters.ObjectID = id
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(filters), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		conv    *domain.Conversation
		actions []domain.ExternalAction
		total   int
	)
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil {
			return e
		}
		if conv == nil {
			return nil
		}
		actions, total, e = tx.ExternalActions.ListByConversation(r.Context(), orgID, conversationID,
			domain.ExternalActionListOpts{
				Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly,
				ObjectID: string(filters.ObjectID), Refused: filters.Refused,
			})
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	out := make([]usage.ActionJSON, len(actions))
	for i, a := range actions {
		out[i] = usage.ToActionJSON(a)
	}
	opConversationActions.Write(w, page, out, total)
}

// artifactJSON is the wire shape of one item of POST
// /api/agent/conversations/{conversationID}/artifacts/list: an object the
// conversation changed (domain.Artifact). Deliverable is the
// conversation's deliverable that is the same object
// (domain.ArtifactDeliverable), or null. Name is the object's name within
// its repository (domain.Artifact.Name), "" for a kind Target already names.
type artifactJSON struct {
	ObjectID    domain.ObjectRef               `json:"object_id" doc:"The object's id, which the conversation's actions list takes as object_id."`
	ObjectKind  string                         `json:"object_kind" doc:"The kind of object: pull_request, issue, repository, branch, label, release, workflow, workflow_run, review, comment, or message."`
	Provider    string                         `json:"provider" doc:"The outside system the object is in, such as github, jira, linear, or slack."`
	Scope       string                         `json:"scope" doc:"Where in that system the object is: the GitHub host, the Jira site, the Linear workspace id, or slack.com."`
	Target      string                         `json:"target" doc:"The key of the object, or of the entity it is on, such as owner/repo#42 or PROJ-123, kept current through renames."`
	Name        string                         `json:"name" doc:"The object's name within its repository for a branch, label, release, workflow, or workflow run, and empty for every other kind."`
	URL         string                         `json:"url" doc:"The object's page in its system, empty when no action recorded one."`
	EntityID    string                         `json:"entity_id,omitempty" doc:"The id of the entity the object is or is on, absent when it is on none."`
	LastAction  string                         `json:"last_action" doc:"The newest action the conversation took on the object, such as pr_created or branch_pushed."`
	ActionCount int                            `json:"action_count" doc:"How many actions the conversation took on the object."`
	FirstAt     time.Time                      `json:"first_at" doc:"When the conversation first acted on the object, as an RFC 3339 timestamp."`
	LastAt      time.Time                      `json:"last_at" doc:"When the conversation last acted on the object, as an RFC 3339 timestamp."`
	Deliverable *usage.ArtifactDeliverableJSON `json:"deliverable" doc:"The conversation's deliverable that is this object, or null when none is."`
}

func toArtifactJSON(a domain.Artifact, deliverables []domain.Deliverable) artifactJSON {
	out := artifactJSON{
		ObjectID:    a.ObjectID,
		ObjectKind:  a.ObjectKind,
		Provider:    a.Provider,
		Scope:       a.Scope,
		Target:      a.Target,
		Name:        a.Name(),
		URL:         a.URL,
		EntityID:    a.EntityID,
		LastAction:  a.LastAction,
		ActionCount: a.ActionCount,
		FirstAt:     a.FirstAt,
		LastAt:      a.LastAt,
	}
	if d, ok := domain.ArtifactDeliverable(a, deliverables); ok {
		out.Deliverable = &usage.ArtifactDeliverableJSON{ID: d.ID, Kind: d.Kind, State: d.State}
	}
	return out
}

// conversationArtifactListRequest is the body of POST
// /api/agent/conversations/{conversationID}/artifacts/list. The conversation
// is the path id and the read has no filters of its own, so the body is
// paging alone.
type conversationArtifactListRequest struct {
	httpx.PageRequest
}

var opConversationArtifacts = httpx.NewListOp[conversationArtifactListRequest, artifactJSON](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/artifacts/list",
	ID:          "list_conversation_artifacts",
	Summary:     "List a conversation's artifacts",
	Description: "Returns one page of the objects in outside systems the conversation changed, one item per object however many actions it took on it, ordered by the latest of those actions first. Each item names the conversation's deliverable that is the same object.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleAgentArtifacts returns the objects this conversation changed,
// one item per object however many actions it took on it, by the latest of
// those actions first. Each item names the conversation's deliverable that is
// the same object, matched after the page is read. Gated as its sibling
// handleAgentActions is: the conversation is read first under request claims,
// so one the caller cannot see is a 404.
//
// POST /api/agent/conversations/{conversationID}/artifacts/list
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

	req, ok := opConversationArtifacts.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		conv         *domain.Conversation
		artifacts    []domain.Artifact
		deliverables []domain.Deliverable
		total        int
	)
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil || conv == nil {
			return e
		}
		artifacts, total, e = tx.Artifacts.ListByConversation(r.Context(), orgID, conversationID,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		if e != nil || !slices.ContainsFunc(artifacts, domain.ArtifactMayHaveDeliverable) {
			return e
		}
		// Every deliverable, not a page of them: the match decides which
		// item names which, and the one that matches is as likely to be
		// past any window.
		deliverables, _, e = tx.Deliverables.ListByConversation(r.Context(), orgID, conversationID, db.Unwindowed)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	out := make([]artifactJSON, len(artifacts))
	for i, a := range artifacts {
		out[i] = toArtifactJSON(a, deliverables)
	}
	opConversationArtifacts.Write(w, page, out, total)
}

// conversationJSON is the conversation resource: the body GET, stop, and
// message answer with, and one row of the conversation list. Keys that only
// some reads compute, or that a read leaves out when its answer is not
// definitive, are omitted rather than zeroed, and a client reads their absence
// as "not known" (see conversationResponse and the add* helpers). A
// conditional value whose zero is itself an answer is a pointer, so omitempty
// drops only the unset key.
type conversationJSON struct {
	ID                  string         `json:"id" doc:"The conversation's id."`
	TaskID              string         `json:"task_id" doc:"The id of the task the conversation works on."`
	PromptID            string         `json:"prompt_id" doc:"The id of the prompt the conversation was started with, empty when it was started with none."`
	Status              string         `json:"status" doc:"The conversation's displayed status: queued, running, open, or failed, or, while an engagement is setting up, its phase: fetching, cloning, agent_starting, or awaiting_credentials."`
	Model               string         `json:"model" doc:"The model the conversation runs on, empty before one is chosen."`
	StartedAt           time.Time      `json:"started_at" doc:"When the conversation was created, as an RFC 3339 timestamp."`
	QueuedAt            *time.Time     `json:"queued_at" doc:"When the conversation last entered the queue, as an RFC 3339 timestamp, or null when it never has."`
	ClaimedAt           *time.Time     `json:"claimed_at" doc:"When work on the conversation last started, as an RFC 3339 timestamp, or null when it never has."`
	CompletedAt         *time.Time     `json:"completed_at" doc:"When the conversation's last engagement finished, as an RFC 3339 timestamp, or null when none has."`
	TotalCostUSD        *float64       `json:"total_cost_usd" doc:"What the conversation's model calls have cost, in USD, or null before any cost is recorded."`
	DurationMs          *int           `json:"duration_ms" doc:"How long the conversation's engagements have spent working, in milliseconds, or null before any time is recorded."`
	NumTurns            *int           `json:"num_turns" doc:"How many turns the conversation's engagements have run, or null before any are recorded."`
	ParkReason          string         `json:"park_reason" doc:"Why the conversation was parked open, such as user_cancelled, stalled, or launch_failed, empty when it is not parked or has resumed since."`
	WorktreePath        string         `json:"worktree_path" doc:"The path of the conversation's run directory on the host that ran it, empty before it has one."`
	ResultSummary       string         `json:"result_summary" doc:"The agent's account of what it did, empty until it gives one."`
	Outcome             string         `json:"outcome" doc:"The verdict the agent reported when it stopped: continue, finish, or abort, empty when it reported none."`
	OutcomeReason       string         `json:"outcome_reason" doc:"Why the agent stopped and what a person needs to do, empty when it gave no reason."`
	FailureKind         string         `json:"failure_kind" doc:"What made a failed conversation fail, such as memory_limit, crash, or executor_lost, empty when it has not failed or the failure was not classified."`
	SessionID           string         `json:"session_id" doc:"The Claude Code session id a conversation on the sdk runtime resumes with, empty when it has none."`
	MemoryMissing       bool           `json:"memory_missing" doc:"Whether the agent has not written its own memory of the conversation."`
	HasRun              bool           `json:"has_run" doc:"Whether the conversation holds an assistant turn."`
	Type                string         `json:"type" doc:"What the conversation is for: delegation for agent work, or interactive for the conversation a person's claim of the task started."`
	Runtime             string         `json:"runtime" doc:"The engine that runs the conversation: sdk or native."`
	TriggerType         string         `json:"trigger_type" doc:"How the conversation was started: manual or event."`
	TriggerID           string         `json:"trigger_id" doc:"The id of the event handler whose firing started the conversation, empty for a manual one."`
	ActorAgentID        string         `json:"actor_agent_id" doc:"The id of the agent the conversation runs as, empty when none is recorded."`
	ActorAgentName      string         `json:"actor_agent_name" doc:"The display name of that agent, empty when actor_agent_id is empty or the agent was deleted."`
	BlueprintRunID      string         `json:"blueprint_run_id" doc:"The id of the blueprint run the conversation is a step of, empty when it is not a step."`
	BlueprintStepIndex  *int           `json:"blueprint_step_index" doc:"The conversation's step in its blueprint run, counted from 0, or null when it is not a step."`
	BlueprintStepCount  int            `json:"blueprint_step_count" doc:"How many steps the conversation's blueprint run has, 0 when it is not a step or the run could not be read."`
	BlueprintRunStatus  string         `json:"blueprint_run_status" doc:"The status of the conversation's blueprint run: running, completed, aborted, failed, or cancelled, empty when it is not a step or the run could not be read."`
	ArtifactCount       int            `json:"artifact_count" doc:"How many objects in outside systems the conversation changed."`
	ArtifactCounts      map[string]int `json:"artifact_counts" doc:"How many objects the conversation changed, keyed by object kind, such as pull_request or branch, and an empty object when it changed none."`
	ActionCount         int            `json:"action_count" doc:"How many actions on outside systems the conversation recorded, refused and failed ones included."`
	InputTokens         int            `json:"input_tokens" doc:"How many input tokens the conversation's model calls used."`
	OutputTokens        int            `json:"output_tokens" doc:"How many output tokens the conversation's model calls produced."`
	CacheReadTokens     int            `json:"cache_read_tokens" doc:"How many input tokens the conversation's model calls read from the provider's prompt cache."`
	CacheCreationTokens int            `json:"cache_creation_tokens" doc:"How many input tokens the conversation's model calls wrote to the provider's prompt cache."`

	CurrentAction       string     `json:"current_action,omitempty" doc:"A sentence saying what a running conversation is doing, present only on the conversation list and only when there is something to say."`
	QueuePosition       *int       `json:"queue_position,omitempty" doc:"The conversation's place in its organization's queue, counted from 1, present only on the conversation list and only while it waits in line."`
	EndedAt             *time.Time `json:"ended_at,omitempty" doc:"When the conversation stopped being its task's current conversation, as an RFC 3339 timestamp, absent while it still is."`
	EndedReason         string     `json:"ended_reason,omitempty" doc:"Why the conversation stopped being its task's current conversation, such as requeued, delegated, or taken_over, present exactly when ended_at is."`
	StopRequestedAt     *time.Time `json:"stop_requested_at,omitempty" doc:"When a stop that has not taken effect yet was requested, as an RFC 3339 timestamp, absent when no stop is pending."`
	NextAttemptAt       *time.Time `json:"next_attempt_at,omitempty" doc:"The earliest time the conversation is picked up again after its model provider was unavailable, as an RFC 3339 timestamp, absent when nothing is holding it back."`
	ClaimLastActivityAt *time.Time `json:"claim_last_activity_at,omitempty" doc:"When the engagement running the conversation last reported activity, as an RFC 3339 timestamp, absent when no engagement holds the conversation or it has not reported yet."`
	ClaimCurrentOp      string     `json:"claim_current_op,omitempty" doc:"The operation that engagement had in flight at its last report, absent when no engagement holds the conversation or it has not reported yet."`

	HasUnresolvedDeliverables *bool    `json:"has_unresolved_deliverables,omitempty" doc:"Whether a draft pull request or a ready review the conversation prepared still waits on a person, absent when the conversation's deliverables could not be read."`
	UnresolvedPRCount         *int     `json:"unresolved_pr_count,omitempty" doc:"How many draft pull requests the conversation prepared still wait on a person, absent when the conversation's deliverables could not be read."`
	UnresolvedReviewCount     *int     `json:"unresolved_review_count,omitempty" doc:"How many ready reviews the conversation prepared still wait on a person, absent when the conversation's deliverables could not be read."`
	PendingDeliverableIDs     []string `json:"pending_deliverable_ids,omitzero" doc:"The ids of the draft pull requests and ready reviews that still wait on a person, an empty array when none do, and absent when the conversation's deliverables could not be read."`

	CacheWarmUntil      string `json:"cache_warm_until,omitempty" doc:"When the model provider's prompt cache from the newest assistant turn expires, as an RFC 3339 timestamp, absent from the conversation list and when the conversation has no assistant turn."`
	Resumable           *bool  `json:"resumable,omitempty" doc:"Whether a message sent now would continue the conversation, absent from the conversation list and for a conversation that is active or failed."`
	ResumeBlockedReason string `json:"resume_blocked_reason,omitempty" doc:"Why a message sent now would not continue the conversation, such as workspace_expired or blueprint_concluded, present exactly when resumable is false."`
	NextMessageModel    string `json:"next_message_model,omitempty" doc:"The model a message that names none would run on, absent from the conversation list and when such a message would be refused."`
	NextMessageEffort   string `json:"next_message_effort,omitempty" doc:"The reasoning effort a message that names none would run at, present with next_message_model unless it is the provider's default."`
	CanWrite            *bool  `json:"can_write,omitempty" doc:"Whether the caller may stop the conversation, message it, and answer its permission prompts, absent when the check fails."`
}

// conversationResponse projects a Conversation into the wire shape the frontend consumes,
// augmented with `artifact_count` and the per-kind `artifact_counts` (so the
// Board card can show what a conversation changed without a per-card fetch) and
// the derived approval signal:
// `has_unresolved_deliverables` (bool), `unresolved_pr_count` /
// `unresolved_review_count`, and `pending_deliverable_ids` (the set of unresolved
// approvable deliverable ids — all draft PRs + all ready reviews — for the per-item
// resolve UI). Approval is not a stored conversation status but a view over the
// unresolved-deliverable set.
//
// Pure projection — it does no I/O. The caller supplies every input, which is
// what lets the conversation-list path batch its reads instead of issuing them
// per conversation:
//   - deliverableCount from Deliverables.CountByConversation (the single-conversation path counts one
//     conversation; the list path batches every conversation in one query — N+1 avoidance).
//     It is not projected: it is what tells a conversation with no deliverables
//     (definitively nothing unresolved) from one whose deliverable read failed.
//   - arts is the conversation's deliverable set, which the caller reads best-effort (only
//     for conversations that have any deliverable, so a conversation with none costs no list).
//   - artifactCounts is the conversation's artifacts by object kind, from
//     Artifacts.CountByConversations (batched like deliverableCount; nil for
//     a conversation with none).
//   - actionCount is the number of the conversation's ledger rows, from
//     ExternalActions.CountByConversations (batched the same way; 0 for a
//     conversation with none).
//   - progress is the owning blueprint run's plan length and status, from
//     Blueprints.RunProgress (batched the same way). Zero values when the
//     conversation has no blueprint, and when the caller could not resolve one
//     — a manual blueprint run belongs to its creator under RLS, so a teammate
//     reads 0 and "" here and gets the unqualified projection, which is what
//     they already get for the chain rail.
//
// The plan length is what makes a step's position legible: paired with
// blueprint_step_index it says whether a concluded step is its chain's last —
// the difference between the task being over and the next step picking it up.
// The run's status is what says whether the work is done at all: a
// conversation never concludes, so its own status cannot.
// Outcome alone cannot say that. It is the vocabulary an agent emits about its
// OWN ending, and what a given value implies for the chain depends on the
// runtime: under the SDK a terminal step reports `finish`, while the native
// loop stamps `continue` on every ordinary completion, final and single-step
// conversations included (internal/agentloop, internal/delegate/blueprint.go's
// blueprintDecisionForStepConversation is what resolves it against position).
//
// The derived approval keys are emitted only when the answer is definitive: when
// the conversation has no deliverables (deliverableCount == 0, so nothing can be unresolved) or
// when the deliverable set is actually in hand (len(arts) > 0). When deliverableCount
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
func conversationResponse(conv *domain.Conversation, deliverableCount int, arts []domain.Deliverable, artifactCounts map[string]int, actionCount int, progress db.RunProgress, currentAction string) conversationJSON {
	out := conversationJSON{
		ID:            conv.ID,
		TaskID:        conv.TaskID,
		PromptID:      conv.PromptID,
		Status:        conv.Status,
		Model:         conv.Model,
		StartedAt:     conv.StartedAt,
		QueuedAt:      conv.QueuedAt,
		ClaimedAt:     conv.ClaimedAt,
		CompletedAt:   conv.CompletedAt,
		TotalCostUSD:  conv.TotalCostUSD,
		DurationMs:    conv.DurationMs,
		NumTurns:      conv.NumTurns,
		ParkReason:    string(conv.ParkReason),
		WorktreePath:  conv.WorktreePath,
		ResultSummary: conv.ResultSummary,
		// The terminal envelope's parsed outcome and its abort note; empty
		// string when the conversation holds none (still executing, an infra
		// failure, or an outcome gate that exhausted its retries).
		Outcome:       conv.Outcome,
		OutcomeReason: conv.OutcomeReason,
		FailureKind:   string(conv.FailureKind),
		SessionID:     conv.SessionID,
		MemoryMissing: conv.MemoryMissing,
		// Whether the conversation holds an assistant message. A conversation a
		// person's claim minted reads false until its first turn.
		HasRun: conv.HasRun,
		Type:   conv.Type,
		// The engine driving the transcript: a composer says when a settings
		// change takes effect, which differs between the two.
		Runtime:            conv.Runtime,
		TriggerType:        conv.TriggerType,
		TriggerID:          conv.TriggerID,
		ActorAgentID:       conv.ActorAgentID,
		ActorAgentName:     conv.ActorAgentName,
		BlueprintRunID:     conv.BlueprintRunID,
		BlueprintStepIndex: conv.BlueprintStepIndex,
		BlueprintStepCount: progress.StepCount,
		BlueprintRunStatus: string(progress.Status),
		// The objects the conversation changed, in all and by object
		// kind, read from the ledger. {} for a conversation that changed
		// nothing.
		ArtifactCount:  artifactTotal(artifactCounts),
		ArtifactCounts: artifactCountsOrEmpty(artifactCounts),
		// How many acts on an outside system the conversation recorded, a
		// refused one included: its ledger rows, whoever performed them. A
		// run acting again on an object it already changed moves this and
		// not the artifact counts, so it is what a view of the
		// conversation's actions refetches on.
		ActionCount: actionCount,
		// The token rollups the conversation read already SUMs, alongside the
		// cost / duration / turns ones above. Plain ints — 0 for a
		// conversation that never streamed a usage-bearing message — so a
		// consumer never has to distinguish absent from none.
		InputTokens:         conv.InputTokens,
		OutputTokens:        conv.OutputTokens,
		CacheReadTokens:     conv.CacheReadTokens,
		CacheCreationTokens: conv.CacheCreationTokens,
		// What a WORKING conversation is doing right now, in place of the
		// title it does not have. Composed by the caller, and emitted only
		// when it had something honest to say — an absent key means the
		// client renders the state label, never a fabricated action.
		CurrentAction: currentAction,
		// The queued run's place in its own org's line, emitted only when the
		// conversation actually has one — a running, open or terminal
		// conversation carries no key at all, never a 0 or a null, so a
		// client renders the mark on presence rather than on a sentinel.
		// Org-local by contract: it carries no fleet occupancy, no executor
		// identity, no placement tier and no cross-org term, and must not
		// grow one. See domain.Conversation.QueuePosition for why a
		// deployment-truthful position would be a cross-tenant disclosure.
		QueuePosition: conv.QueuePosition,
		// A pending stop: asked for, not yet settled. Absent rather than null
		// once settled or never asked, read off presence like ended_at. Who
		// asked stays off the wire; the park_reason the settlement records
		// says it.
		StopRequestedAt: conv.StopRequestedAt,
		// The wait a hand-back set: the earliest the conversation is claimed
		// again, after its model provider was unavailable. It still reads
		// `queued`, with no queue_position, because it waits for a time
		// rather than for the conversations ahead of it. Absent when nothing
		// is holding it, read off presence like the keys above.
		NextAttemptAt: conv.NextAttemptAt,
		// The live claim's activity as its last renewal stamped it: when the
		// engagement last did anything its stall watchdog counts, and what it
		// had in flight. Absent with no live claim or before the first
		// renewal, read off presence like the keys above.
		ClaimLastActivityAt: conv.ClaimLastActivityAt,
		ClaimCurrentOp:      conv.ClaimCurrentOp,
	}
	// The boundary: when this conversation stopped being its task's live one,
	// and why. Both keys are absent — not null, not "" — while it is open, so
	// a client reads "ended" off presence rather than off a sentinel, the same
	// way it reads queue_position. They travel together by construction: the
	// stamp writes both columns or neither.
	if conv.EndedAt != nil {
		out.EndedAt = conv.EndedAt
		out.EndedReason = string(conv.EndedReason)
	}
	if deliverableCount == 0 || len(arts) > 0 {
		prCount, reviewCount := domain.UnresolvedDeliverableCounts(arts)
		hasUnresolved := prCount > 0 || reviewCount > 0
		out.HasUnresolvedDeliverables = &hasUnresolved
		out.UnresolvedPRCount = &prCount
		out.UnresolvedReviewCount = &reviewCount
		// The set of unresolved approvable deliverable ids (all draft PRs + all ready
		// reviews) — what the approval UI lists for per-item resolve. Emitted under
		// the same definitive-only guard as the counts above, and always non-nil so
		// the field is [] rather than null when nothing is unresolved.
		out.PendingDeliverableIDs = domain.UnresolvedDeliverableIDs(arts)
	}
	return out
}

// artifactTotal sums an artifact count by kind.
func artifactTotal(byKind map[string]int) int {
	n := 0
	for _, c := range byKind {
		n += c
	}
	return n
}

// artifactCountsOrEmpty is byKind, or {} rather than null when it is nil.
func artifactCountsOrEmpty(byKind map[string]int) map[string]int {
	if byKind == nil {
		return map[string]int{}
	}
	return byKind
}

// transcriptListRequest is the body of POST
// /api/agent/conversations/{conversationID}/messages/list.
//
// after_id is a watermark the client already holds: every row up to it is
// on its screen, and it wants only what came after. The repair poll that
// patches a transcript assembled from websocket frames sends it. It is a
// filter on the list, never a cursor: the token only continues a walk, and
// the walk runs one way whatever the filter, from the newest rows toward the
// oldest.
type transcriptListRequest struct {
	AfterID int `json:"after_id" doc:"The id of the newest message the caller already holds, to list only the messages after it; absent or 0 lists every message."`

	httpx.PageRequest
}

// transcriptFilterKey is what a transcript page token is fingerprinted
// against: the conversation the path names and the watermark.
type transcriptFilterKey struct {
	ConversationID string `json:"conversation_id"`
	AfterID        int    `json:"after_id"`
}

var opConversationMessages = httpx.NewListOp[transcriptListRequest, domain.MessageDTO](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/messages/list",
	ID:          "list_conversation_messages",
	Summary:     "List a conversation's messages",
	Description: "Returns one page of the conversation's transcript, narrowed to the messages after after_id when it is given. The first page holds the newest messages and each next page older ones, and every page lists its messages oldest first; total_count counts every message the filter admits.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	req, ok := opConversationMessages.Decode(w, r)
	if !ok {
		return
	}
	// A negative watermark would reach the store as `id > -N`, which selects
	// the whole transcript only because ids start at 1 — a coincidence, not a
	// contract — so it is refused rather than read as none.
	var v httpx.Validation
	if req.AfterID < 0 {
		v.Invalid("after_id", "after_id must be a message id, or 0 for every message")
	}
	page := httpx.ResolveKeysetPage(&v, req.PageRequest, httpx.FilterFingerprint(transcriptFilterKey{
		ConversationID: conversationID, AfterID: req.AfterID,
	}), 0)
	// The key is the oldest row of the page before, the ceiling this page
	// reads below.
	beforeID := 0
	if len(page.After) > 0 {
		n, err := strconv.Atoi(page.After[0])
		if err != nil || n <= 0 || len(page.After) != 1 {
			v.Param("page_token", "page_token does not address this list's ordering; restart from the first page")
		}
		beforeID = n
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// One row past the window answers whether an older page follows, which
	// a keyset page cannot derive from its total.
	window := db.MessageWindow{SinceID: req.AfterID, BeforeID: beforeID, CountOnly: page.CountOnly}
	if page.Limit > 0 {
		window.Limit = page.Limit + 1
	}
	var (
		conv     *domain.Conversation
		messages []domain.Message
		total    int
	)
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// The messages read alone cannot tell an invisible or nonexistent
		// conversation from an empty transcript: RLS hides the rows either way.
		conv, e = tx.Conversations.Get(r.Context(), orgID, conversationID)
		if e != nil || conv == nil {
			return e
		}
		messages, total, e = tx.Conversations.MessagesWindow(r.Context(), orgID, conversationID, window)
		return e
	}); err != nil {
		internalError(w, "agent", err)
		return
	}
	if conv == nil {
		notFound(w, "conversation")
		return
	}

	// The window comes back oldest-first, so the row past it is the first.
	var nextKey []string
	if page.Limit > 0 && len(messages) > page.Limit {
		messages = messages[1:]
		nextKey = []string{strconv.Itoa(messages[0].ID)}
	}
	opConversationMessages.WriteKeyset(w, page, domain.MessageDTOs(messages), total, nextKey)
}

var opConversationStop = httpx.NewJSONOp[httpx.NoBody, conversationJSON](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/stop",
	ID:          "stop_conversation",
	Summary:     "Stop a conversation",
	Description: "Requests that the conversation's agent stop, and returns the conversation with stop_requested_at set. The conversation parks open once its engagement settles the stop, which leaves it resumable, and its blueprint run and task are left as they are. A second request while the first is pending answers the same.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleAgentStop is the conversation-level stop verb: the agent stops, the
// conversation parks `open`, and everything outside it freezes — the blueprint
// keeps its status and the task keeps its disposition, which is what leaves the
// parked conversation resumable. Cancelling a plan is the blueprint endpoint's
// job; dispositioning work is the task gestures'.
//
// The route records the request and answers with the conversation as it then
// reads: stop_requested_at set, and the status still whatever it was. The
// park arrives on the conversation_update push when the holder, or the
// dispatcher for a conversation nobody holds, settles it. A second POST while
// the first is pending is the same 200 — the request is idempotent.
//
// It is the single address for the gesture. The former /cancel and /interrupt
// are gone rather than aliased: two endpoints is how one gesture grew two
// meanings of `open` that a user could only tell apart by which button they
// pressed.
//
// The caller is checked by requireWritableConversation: 404 for a
// conversation they cannot see, 403 for one they may only read. A
// conversation the caller may act on that has already concluded is 409
// ALREADY_TERMINAL, the same answer its sibling /message gives for the same
// state. Anything else Stop returns is an internal fault on the way to stopping a conversation
// that really was active — a failed read, a failed intent write — and goes through
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
	traceConversation(r, conversationID)
	spawner := ag.spawner()
	if spawner == nil {
		httpx.WriteDelegationUnavailable(w)
		return
	}
	if !ag.requireWritableConversation(w, r, orgID, userID, conversationID) {
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
	ag.writeConversationResource(w, r, opConversationStop.Write, orgID, userID, conversationID)
}

// conversationViewOnlyMessage is the 403 body for a caller who can read a
// conversation and may not act on it.
const conversationViewOnlyMessage = "view-only access: you can read this conversation but not stop it, message it, or answer its prompts"

// requireWritableConversation is the gate every route that acts on a
// conversation runs first: stop, message, and a permission answer. Reading a
// conversation does not imply acting on it, because an agent acts with its
// team's credentials, so the rule is the conversations_update policy's
// (authz.Checker.WritableConversations): a team viewer, an org member who is
// not an admin on an org-visible conversation, and anyone but the creator of
// a private one may read and may not act.
//
// The two refusals follow the disclosure rule. A conversation the caller
// cannot see is 404, the same as one that never existed; one they can see and
// may not act on is 403, since 404 for a row they can open reads as a bug.
// Visibility is read first so the 403 never answers for an id the caller
// cannot see.
//
// The steering ops resolve a conversation by id against in-memory state (the
// process registry, the permission broker), which carries no claims, so this
// gate is the whole authorization for them. It answers before anything is
// recorded or broadcast. Writes a response and returns false when the request
// may not proceed.
func (ag *agentHandler) requireWritableConversation(w http.ResponseWriter, r *http.Request, orgID, userID, conversationID string) bool {
	visible, err := ag.conversationVisible(r.Context(), orgID, userID, conversationID)
	if err != nil {
		internalError(w, "agent", err)
		return false
	}
	if !visible {
		notFound(w, "conversation")
		return false
	}
	canWrite, err := ag.az.UserCanWriteConversation(r.Context(), userID, orgID, conversationID)
	if err != nil {
		internalError(w, "agent", fmt.Errorf("conversation-write check for %s: %w", conversationID, err))
		return false
	}
	if !canWrite {
		httpx.Forbidden(w, conversationViewOnlyMessage)
		return false
	}
	return true
}

// conversationVisible reports whether conversationID exists and is visible to
// the caller, read under their claims (RLS in multi mode).
func (ag *agentHandler) conversationVisible(ctx context.Context, orgID, userID, conversationID string) (bool, error) {
	var exists bool
	err := ag.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		conv, e := tx.Conversations.Get(ctx, orgID, conversationID)
		if e != nil {
			return e
		}
		exists = conv != nil
		return nil
	})
	return exists, err
}

// conversationMessageRequest is the body of POST
// /api/agent/conversations/{conversationID}/message.
type conversationMessageRequest struct {
	Text   string  `json:"text" doc:"The message, which must not be blank."`
	Model  *string `json:"model" doc:"The model the message runs on, one this deployment offers; absent or null runs it on the model the conversation's next_message_model names."`
	Effort *string `json:"effort" doc:"The reasoning effort the message runs at: low, medium, high, xhigh, or max; absent or null runs it at the effort the conversation's next_message_effort names, or the provider's default when that is absent."`
}

var opConversationMessage = httpx.NewJSONOp[conversationMessageRequest, conversationJSON](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/message",
	ID:          "message_conversation",
	Summary:     "Send a message to a conversation",
	Description: "Sends a person's message to the conversation's agent, which takes it in its current turn when it is running or starts working on it again when it is parked, and returns the conversation. A conversation that can take no message is refused with nothing recorded.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintYes},
})

// handleMessage routes a free-form user message to a conversation: a live conversation is
// steered in place, a parked conversation is woken via resume. requireWritableConversation
// runs before any control op: 404 for a conversation the caller cannot see, 403 for one
// they may only read, and neither records or broadcasts the message.
//
// Recording belongs to the delegate layer, and it happens only when the
// message actually goes somewhere: a live steer records and broadcasts the
// row after the process takes the text, a follow-up queues it as the
// undelivered row a claim will drain. A conversation that can take no message
// (terminal / no live process) is 409 with nothing recorded and nothing
// broadcast — an optimistic write here used to leave a refused send painted
// onto every watcher's transcript as a message the agent never received,
// while the sender simultaneously saw an error toast.
//
// The body is {text, model?, effort?}. model and effort are the settings the
// message runs on, and one left out takes the value the conversation's newest
// person message ran on. A model must be one this deployment offers and an
// effort one of domain.EffortLevels; whether the team may run the model, and
// whether the model takes the effort, the send decides against the settings it
// resolves (409, naming the field).
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
	traceConversation(r, conversationID)

	body, ok := opConversationMessage.Decode(w, r)
	if !ok {
		return
	}
	settings, ok := decodeTurnSettings(w, body.Text, body.Model, body.Effort)
	if !ok {
		return
	}

	spawner := ag.spawner()
	if spawner == nil {
		httpx.WriteDelegationUnavailable(w)
		return
	}

	if !ag.requireWritableConversation(w, r, orgID, userID, conversationID) {
		return
	}

	if err := spawner.SendMessage(r.Context(), orgID, conversationID, userID, body.Text, settings); err != nil {
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
		// The 4xx arms are all delegate and modelaccess sentinels —
		// author-written state descriptions with nothing internal in them,
		// and the text is the answer ("this conversation has moved on"), so
		// it is surfaced.
		httpx.WriteErrors(w, status, httpx.ErrorItem{Reason: reason, Message: err.Error(), Field: steerErrorField(err)})
		return
	}
	ag.writeConversationResource(w, r, opConversationMessage.Write, orgID, userID, conversationID)
}

// decodeTurnSettings checks a message body's fields and answers the settings
// the message names, reporting every failing field, text included, in one 400.
// A model must be a key this deployment offers, the check the team-settings
// PATCH makes for ai_model, and an effort one of domain.EffortLevels. A field
// that is absent or null is not given, and the send resolves it.
func decodeTurnSettings(w http.ResponseWriter, text string, model, effort *string) (domain.TurnSettings, bool) {
	var v httpx.Validation
	if strings.TrimSpace(text) == "" {
		v.Missing("text")
	}
	var settings domain.TurnSettings
	if model != nil {
		if !models.DeploymentUniverse().Offers(*model) {
			v.Invalid("model", fmt.Sprintf("model %q is not one this deployment offers (%s)", *model, strings.Join(models.DeploymentUniverse().Keys(), ", ")))
		}
		settings.Model = *model
	}
	if effort != nil {
		if !domain.ValidEffort(*effort) {
			v.Invalid("effort", fmt.Sprintf("effort must be one of: %s", strings.Join(domain.EffortLevels, ", ")))
		}
		settings.Effort = *effort
	}
	if v.Flush(w, http.StatusBadRequest) {
		return domain.TurnSettings{}, false
	}
	return settings, true
}

// steerErrorField is the body field a send refusal is about: the one a
// settings refusal names, and model for a model the team may not run.
func steerErrorField(err error) string {
	var fe *delegate.SettingsFieldError
	if errors.As(err, &fe) {
		return fe.Field
	}
	if errors.Is(err, domain.ErrModelNotEnabled) {
		return "model"
	}
	return ""
}

// steerErrorStatus maps a SendMessage error to an HTTP status. A
// conversation that can't take the op right now — no live process (ErrNoLiveProcess),
// not steerable (ErrConversationNotSteerable), or moved out of a resumable state
// (ErrConversationNotResumable) — is 409 Conflict so the client refreshes and re-reads
// the conversation's state. Two wakes racing is NOT among them: the loser's message is
// queued alongside the winner's and delivered by the winner's claim, so it
// returns nil and the client is told "sent", which is what happened. A wake
// that loses to a conversation going terminal still 409s — nothing will claim
// it, so the message is not queued. An expired workspace (ErrWorkspaceExpired) is 410 Gone: the
// conversation's saved state was reaped after the retention window, so retrying won't
// help — the client surfaces the clear error rather than a transient conflict.
// A message whose settings the send refuses is 409 too, naming the field:
// a model the conversation's team may not run (domain.ErrModelNotEnabled), a
// model or effort other than a running blueprint step's
// (delegate.ErrStepSettingsFixed), an effort the model does not take
// (delegate.ErrEffortNotSupported), and a switch to a model whose window the
// conversation already fills (delegate.ErrModelWindowTooSmall). Each is about
// the conversation as it stands, and a different pick on the next message is
// the fix, so the client re-reads the conversation rather than being told the
// body was malformed.
//
// A model the organization cannot authenticate is 409 NOT_CONFIGURED, the
// status delegate and the settings saves give it: a model whose provider is
// not connected (modelaccess.ErrProviderUnconfigured), which names the model
// field, and an organization that brings its own credentials and has
// connected none (modelaccess.ErrNoCredentials), which names no field because
// no choice of model fixes it. A message to a blueprint step names no field
// for either, because its model cannot change.
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
		errors.Is(err, delegate.ErrBlueprintCancelled),
		errors.Is(err, delegate.ErrConversationEnded):
		return http.StatusConflict, httpx.ReasonAlreadyTerminal
	case errors.Is(err, modelaccess.ErrProviderUnconfigured),
		errors.Is(err, modelaccess.ErrNoCredentials):
		return http.StatusConflict, httpx.ReasonNotConfigured
	case errors.Is(err, delegate.ErrNoLiveProcess),
		errors.Is(err, delegate.ErrConversationNotSteerable),
		errors.Is(err, delegate.ErrConversationNotResumable),
		errors.Is(err, delegate.ErrStepHandedOff),
		errors.Is(err, domain.ErrModelNotEnabled),
		errors.Is(err, delegate.ErrStepSettingsFixed),
		errors.Is(err, delegate.ErrEffortNotSupported),
		errors.Is(err, delegate.ErrModelWindowTooSmall):
		return http.StatusConflict, httpx.ReasonConflict
	default:
		return http.StatusInternalServerError, httpx.ReasonInternal
	}
}

// handleAgentPermission answers a pending tool-permission prompt a conversation surfaced
// via a `permission_request` WS event. Body: {"behavior":"allow"|"deny",
// "message"?:string,"updated_input"?:object}. The path's tool call id is the
// tool_use id the prompt was raised for. requireWritableConversation runs first,
// as it does for stop and message: a conversation the caller cannot see is 404,
// and one they may only read is 403. A prompt that isn't pending (already
// answered, timed out, or never existed) is also 404. A 200 always answers with
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
	traceConversation(r, conversationID)
	toolCallID := r.PathValue("toolCallID")

	body, ok := opConversationPermissionResolve.Decode(w, r)
	if !ok {
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
		httpx.WriteDelegationUnavailable(w)
		return
	}

	// Authorize the conversation before touching the broker — the broker's own
	// org check is a backstop, not a team-level gate.
	if !ag.requireWritableConversation(w, r, orgID, userID, conversationID) {
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
		if txErr := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	opConversationPermissionResolve.Write(w, agentPermissionResolvedResponse{Status: "resolved", Permission: resolved})
}

// agentPermissionResolvedResponse is handleAgentPermission's one 200 body
// shape. Permission is nil exactly when there's no audit row to answer with
// (see handleAgentPermission's doc) — never represented by swapping in a
// differently-shaped body, so a client can always safely read Status without
// a type switch.
type agentPermissionResolvedResponse struct {
	Status     string                         `json:"status" doc:"Always resolved."`
	Permission *domain.ConversationPermission `json:"permission,omitempty" doc:"The answered prompt, absent when no record of it could be read."`
}

// agentPermissionAnswerRequest is the body of POST
// /api/agent/conversations/{conversationID}/permissions/{toolCallID}.
// Behavior decides which of the other two fields applies: Message to a deny,
// UpdatedInput to an allow.
type agentPermissionAnswerRequest struct {
	Behavior     string         `json:"behavior" doc:"The answer: allow or deny."`
	Message      string         `json:"message" doc:"On a deny, the reason the agent is given, which may be empty."`
	UpdatedInput map[string]any `json:"updated_input" doc:"On an allow, the tool input the agent runs the call with in place of its own; absent or null runs the call with the agent's input."`
}

var opConversationPermissionResolve = httpx.NewJSONOp[agentPermissionAnswerRequest, agentPermissionResolvedResponse](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/{conversationID}/permissions/{toolCallID}",
	ID:          "resolve_conversation_permission",
	Summary:     "Answer a conversation's permission prompt",
	Description: "Allows or denies the tool call a pending permission prompt asks about, which lets the conversation's agent go on with its turn. behavior decides which other field applies: message to a deny, updated_input to an allow. A prompt that is no longer pending is not found.",
	PathParams: map[string]string{
		"conversationID": "The conversation's id.",
		"toolCallID":     "The id of the tool call the prompt asks about, as the pending prompt's tool_call_id gives it.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

var opConversationPermissions = httpx.NewJSONOp[httpx.NoBody, []domain.PendingPermissionDTO](httpx.OpSpec{
	Pattern:     "GET /api/agent/conversations/{conversationID}/permissions",
	ID:          "list_conversation_permissions",
	Summary:     "List a conversation's pending permission prompts",
	Description: "Returns every permission prompt the conversation's agent is waiting on a person to answer, as an array with no paging, each with the time it has left.",
	PathParams:  map[string]string{"conversationID": "The conversation's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
// A read, so it asks only that the caller can see the conversation: a reader
// who may not answer the prompts still learns that the agent is waiting. A
// conversation the caller can't see is 404 rather than an empty list (which
// would confirm the id exists).
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
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	// The rows decide which prompts are pending; the broker, when this
	// process holds the wait, decides how long each has left. A nil spawner
	// has no waits to report, which is a fallback to the rows rather than a
	// reason to fail a read that never needed it.
	var live map[string]time.Duration
	if spawner := ag.spawner(); spawner != nil {
		live = spawner.PermissionRemaining(orgID, conversationID)
	}
	opConversationPermissions.Write(w, domain.PendingPermissionDTOs(pending, time.Now().UTC(), live))
}

// enrichConversations projects conversations onto the wire shape, augmenting each with a batched
// artifact_count and the derived has_unresolved_deliverables (+ per-kind counts).
// Both deliverable reads stay O(1) in queries regardless of how many conversations the set
// has. Board's useWebSocket re-fetches on every status transition, so a per-conversation
// read would scale with the task's conversation history.
//
//   - the deliverable count for every conversation: one CountByConversation (conversationID→count; a
//     conversation with no deliverables is absent → 0). Not projected; it decides which
//     conversations need the list below.
//   - artifact_count and artifact_counts for every conversation: one
//     Artifacts.CountByConversations (conversation → object kind → count).
//   - action_count for every conversation: one
//     ExternalActions.CountByConversations.
//   - has_unresolved_deliverables for the conversations that have deliverables: one ListByConversations
//     over just those conversation ids (count>0), grouped per conversation. The derivation needs
//     the actual deliverables, but a conversation with none can't have an unresolved one, so
//     it costs no list.
//   - blueprint_step_count for the conversations that belong to a blueprint: one
//     RunProgress over the deduped blueprint_run ids.
//   - current_action for the conversations displaying as `running`: one
//     NewestAssistantToolCallsForConversations over just those ids, composed
//     into prose per current_action.go. Only running conversations are read —
//     every other state already has an honest label of its own, and the newest
//     tool call on a stopped conversation describes something that is no longer
//     happening.
//
// Deliberately best-effort: a ListByConversations, RunProgress or
// NewestAssistantToolCallsForConversations failure drops what that read
// contributes (logged) but leaves counts and the rest intact. These
// are display annotations on rows the caller can already see, so surfacing the
// failure would fail a whole board refresh over a badge. Shared by the
// single-task and batched (task_ids) conversation-list paths.
func enrichConversations(ctx context.Context, tx db.TxStores, orgID string, convs []domain.Conversation) ([]conversationJSON, error) {
	conversationIDs := make([]string, len(convs))
	for i := range convs {
		conversationIDs[i] = convs[i].ID
	}
	counts, err := tx.Deliverables.CountByConversation(ctx, orgID, conversationIDs)
	if err != nil {
		return nil, err
	}
	artifactCounts, err := tx.Artifacts.CountByConversations(ctx, orgID, conversationIDs)
	if err != nil {
		return nil, err
	}
	actionCounts, err := tx.ExternalActions.CountByConversations(ctx, orgID, conversationIDs)
	if err != nil {
		return nil, err
	}
	var withDeliverables []string
	for i := range convs {
		if counts[convs[i].ID] > 0 {
			withDeliverables = append(withDeliverables, convs[i].ID)
		}
	}
	artsByConv := map[string][]domain.Deliverable{}
	if len(withDeliverables) > 0 {
		if arts, lerr := tx.Deliverables.ListByConversations(ctx, orgID, withDeliverables); lerr != nil {
			serverLog.Warn("deliverable batch lookup failed; omitting has_unresolved_deliverables", "error", lerr)
		} else {
			for _, a := range arts {
				artsByConv[a.ConversationID] = append(artsByConv[a.ConversationID], a)
			}
		}
	}
	// One read for every blueprint in the set, keyed by blueprint_run id and
	// deduped first (a chain's steps all name the same one). Best-effort: a
	// failure leaves every blueprint_step_count at 0 and every
	// blueprint_run_status empty, which read as "unknown" and cost the
	// qualifier, not the row.
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
	progress := map[string]db.RunProgress{}
	if len(blueprintRunIDs) > 0 {
		if runs, lerr := tx.Blueprints.RunProgress(ctx, orgID, blueprintRunIDs); lerr != nil {
			serverLog.Warn("blueprint run progress batch lookup failed; omitting blueprint_step_count and blueprint_run_status", "error", lerr)
		} else {
			progress = runs
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
	out := make([]conversationJSON, len(convs))
	for i := range convs {
		out[i] = conversationResponse(&convs[i], counts[convs[i].ID], artsByConv[convs[i].ID],
			artifactCounts[convs[i].ID], actionCounts[convs[i].ID], progress[convs[i].BlueprintRunID],
			currentAction(toolCalls[convs[i].ID], convs[i].WorktreePath))
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
// this size, an entity list of maxEntityListIDs, and the status placeholders.
// Rejected rather than truncated, like the task cap above, because a silently
// dropped team is a count that reads as real work disappearing.
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
	TaskIDs []string `json:"task_ids" doc:"The ids of the tasks whose conversations to return, at most 500; absent or empty applies no task filter."`
	// TeamIDs narrows to the conversations these teams own. Optional: absent
	// or empty is no team narrowing — the viewer-wide read the shell rail
	// asks for. A present set that is unusable is a 400, never a silent
	// widening back to every team; the Overview's team-scoped counts would
	// otherwise report another team's work as this team's. When present, at
	// most maxBatchTeamIDs, each a valid team id.
	TeamIDs []string `json:"team_ids" doc:"The ids of the teams whose conversations to return, at most 100; absent or empty applies no team filter."`
	// EntityIDs narrows to the conversations whose task is on one of these
	// entities, for a caller that holds the entities it shows and wants the
	// work on them. Optional: absent or empty is no entity narrowing. When
	// present, at most maxEntityListIDs, each in canonical form. The rows
	// still group by task id, and each row's task_id is the key a caller
	// joins its tasks on.
	EntityIDs []string `json:"entity_ids" doc:"The ids of the entities whose tasks' conversations to return, at most 200; absent or empty applies no entity filter."`
	// Statuses narrows to these DISPLAY statuses — the value every surface
	// shows, which is why a claim phase is a legal value here and the stored
	// column is not what gets filtered. Empty = every status. An unknown value
	// is a client fault, not an empty page.
	Statuses []string `json:"statuses" doc:"The displayed statuses to return, from queued, running, open, failed, fetching, cloning, agent_starting, and awaiting_credentials; absent or empty returns every status."`
	// Attention keeps only the conversations waiting on a human: an unanswered
	// permission prompt, or a not-live conversation still holding an unresolved
	// deliverable. False narrows nothing.
	Attention bool `json:"attention" doc:"Whether to return only the conversations waiting on a person: those with an unanswered permission prompt, and those neither running nor setting up that still hold a draft pull request or a ready review waiting on a person."`
	// IncludeMessages adds each task's PRIMARY (newest-started) conversation's
	// transcript, keyed by that conversation's id — what the Board seeds onto a card.
	IncludeMessages bool `json:"include_messages" doc:"Whether to return, as messages, the transcript of the newest conversation of each task on the page."`

	httpx.PageRequest
}

type conversationListFilterKey struct {
	TaskIDs         []string `json:"task_ids"`
	TeamIDs         []string `json:"team_ids"`
	EntityIDs       []string `json:"entity_ids"`
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
type conversationListResponse struct {
	Runs          map[string][]conversationJSON  `json:"runs" doc:"The page's conversations, keyed by task id, each task's newest first; an empty object when the page holds none."`
	Messages      map[string][]domain.MessageDTO `json:"messages,omitempty" doc:"The transcript of each task's newest conversation on the page, keyed by conversation id, oldest message first, present only when include_messages is true and a transcript holds a message."`
	NextPageToken string                         `json:"next_page_token,omitempty" doc:"The token that reads the next page, as page_token, absent on the last page."`
	TotalCount    int                            `json:"total_count" doc:"How many conversations match the filters, across every page."`
}

var opConversationList = httpx.NewJSONOp[conversationListRequest, conversationListResponse](httpx.OpSpec{
	Pattern:     "POST /api/agent/conversations/list",
	ID:          "list_conversations",
	Summary:     "List conversations",
	Description: "Returns one page of the conversations the caller can see, narrowed by the body's filters and grouped by task id in runs, with each task's newest first. total_count counts conversations, not tasks. With include_messages, messages carries the transcript of each task's newest conversation on the page.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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

	req, ok := opConversationList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	taskIDs := httpx.CanonicalStrings(req.TaskIDs)
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
	teamIDs := httpx.CanonicalStrings(req.TeamIDs)
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
	entityIDs := canonicalIDList(&v, "entity_ids", "an entity", req.EntityIDs)
	statuses := httpx.CanonicalStrings(req.Statuses)
	validStatuses := domain.AllConversationStatuses()
	for _, st := range statuses {
		if !slices.Contains(validStatuses, st) {
			v.Invalid("statuses", fmt.Sprintf("unknown status %q; must be one of: %s",
				st, strings.Join(validStatuses, ", ")))
		}
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(conversationListFilterKey{
		TaskIDs: taskIDs, TeamIDs: teamIDs, EntityIDs: entityIDs, Statuses: statuses,
		Attention: req.Attention, IncludeMessages: req.IncludeMessages,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	resp := conversationListResponse{Runs: map[string][]conversationJSON{}}
	if req.IncludeMessages {
		resp.Messages = map[string][]domain.MessageDTO{}
	}
	var (
		pageIDs  []string
		pageRows []conversationJSON
	)
	if err := ag.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		convs, total, e := tx.Conversations.List(r.Context(), orgID,
			db.ConversationListFilter{TaskIDs: taskIDs, TeamIDs: teamIDs, EntityIDs: entityIDs, Statuses: statuses, Attention: req.Attention},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		if e != nil {
			return e
		}
		resp.TotalCount = total
		// One enrichConversations over the whole page keeps the deliverable reads O(1)
		// regardless of task/conversation count; the index alignment lets us group the
		// projected responses by task without re-projecting.
		enriched, e := enrichConversations(r.Context(), tx, orgID, convs)
		if e != nil {
			return e
		}
		pageRows = enriched
		pageIDs = make([]string, len(convs))
		for i := range convs {
			pageIDs[i] = convs[i].ID
		}
		var primaryConversationIDs []string
		seenTask := map[string]bool{}
		for i := range convs {
			tid := convs[i].TaskID
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
	ag.addCanWrite(r.Context(), orgID, userID, pageIDs, pageRows)
	// Grouped after the write probe, because the groups hold copies of the
	// rows the probe fills in.
	for _, row := range pageRows {
		resp.Runs[row.TaskID] = append(resp.Runs[row.TaskID], row)
	}
	// Counted over the rows the page carries, so the next-token arithmetic
	// sees the same number a flat `items` array would have.
	resp.NextPageToken = httpx.NextPageToken(page, len(pageRows), resp.TotalCount)
	opConversationList.Write(w, resp)
}

// WSHub returns the websocket hub for use by the delegation spawner.
func (s *Server) WSHub() *websocket.Hub {
	return s.ws
}

func (s *Server) registerAgentRoutes() {
	ag := &agentHandler{tx: s.tx, az: s.az, ws: s.ws, spawner: func() *delegate.Spawner { return s.spawner }, reconciler: func() *reconcile.Reconciler { return s.reconciler }}
	s.op(opConversationGet, ag.handleAgentStatus)
	s.op(opConversationMessages, ag.handleMessages)
	// Conversation-scoped deliverable read: the conversation's deliverables across
	// every kind, team-scoped via the conversation. Backs the conversation
	// detail surface.
	s.op(opConversationDeliverables, ag.handleAgentDeliverables)
	// Conversation-scoped external-action read — the audit log filtered to
	// this conversation. Its sibling above answers "what objects does this
	// conversation own"; this answers "what did it do", including the writes
	// that produce no object at all (a review-thread reply, a refused merge, a
	// denied push).
	s.op(opConversationActions, ag.handleAgentActions)
	// Conversation-scoped artifact read — the objects this conversation
	// changed, read from the same ledger as the action list above and
	// grouped per object.
	s.op(opConversationArtifacts, ag.handleAgentArtifacts)
	// The one conversation-level stop. A second address for it is how `open`
	// acquires two meanings.
	s.op(opConversationStop, ag.handleAgentStop)
	s.op(opConversationMessage, ag.handleMessage)
	// The pending set behind the demoted `permission_request` frame: the frame
	// carries only the tool_call_id and every surface reads the prompt from
	// here, so a refresh / second tab / cold load reconstructs it.
	s.op(opConversationPermissions, ag.handleAgentPermissions)
	s.op(opConversationPermissionResolve, ag.handleAgentPermission)
	// Tier-2 conversation-scoped deliverable reconcile: the conversation view
	// polls this while open to refresh that conversation's non-terminal
	// deliverables against GitHub.
	s.op(opConversationDeliverablesRefresh, ag.handleDeliverableRefresh)
	s.op(opConversationList, ag.handleConversations)
}
