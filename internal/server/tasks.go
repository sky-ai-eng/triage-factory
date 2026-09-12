package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// taskJSON is the API representation of a task. Maps entity-joined fields
// to the frontend's expected shape for backward compatibility.
type taskJSON struct {
	ID                  string   `json:"id"`
	EntityID            string   `json:"entity_id"`   // FK to entities.id — lets callers correlate tasks back to their entity
	Source              string   `json:"source"`      // from entity
	SourceID            string   `json:"source_id"`   // from entity
	SourceURL           string   `json:"source_url"`  // from entity
	Title               string   `json:"title"`       // from entity
	EntityKind          string   `json:"entity_kind"` // "pr" | "issue"
	EventType           string   `json:"event_type"`
	DedupKey            string   `json:"dedup_key,omitempty"`
	Severity            string   `json:"severity,omitempty"`
	RelevanceReason     string   `json:"relevance_reason,omitempty"`
	ScoringStatus       string   `json:"scoring_status"`
	CreatedAt           string   `json:"created_at"`
	Status              string   `json:"status"`
	PriorityScore       *float64 `json:"priority_score"`
	AutonomySuitability *float64 `json:"autonomy_suitability"`
	AISummary           string   `json:"ai_summary,omitempty"`
	PriorityReasoning   string   `json:"priority_reasoning,omitempty"`
	CloseReason         string   `json:"close_reason,omitempty"`
	// SnoozeUntil — populated when the task is in a snoozed state.
	// Under the "snoozed ↔ unclaimed" invariant,
	// this is only ever set on queue-lane tasks. Any claim-axis
	// transition wakes the task atomically (clears snooze_until +
	// flips status='snoozed' → 'queued'), so claimed cards on the
	// Board never carry a snooze. The Cards triage view renders
	// future-snoozed entries hidden via the TaskListFilter status filter;
	// the Board's Queue lane could optionally render them at the
	// tail with a "wakes Mar 5" badge (deferred UI follow-up).
	SnoozeUntil string `json:"snooze_until,omitempty"`
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal. Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count"`
	// Claim cols: exposed so the per-card assignee picker
	// can render the current assignee without a second round-trip.
	// Exactly one is set when claimed; both empty when unclaimed.
	// omitempty keeps the wire shape clean for the unclaimed-queue case.
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty"`
	ClaimedByUserID  string `json:"claimed_by_user_id,omitempty"`
	// TeamID is the task's owning team. Exposed so the multi-team board
	// can color-code / tag rows by team. Always set; the
	// frontend only surfaces it when the viewer belongs to ≥2 teams.
	// TODO: board row color-coding consumes this.
	TeamID string `json:"team_id,omitempty"`
}

func taskToJSON(t domain.Task) taskJSON {
	snoozeUntil := ""
	if t.SnoozeUntil != nil {
		snoozeUntil = t.SnoozeUntil.Format(time.RFC3339)
	}
	return taskJSON{
		ID:                  t.ID,
		EntityID:            t.EntityID,
		Source:              t.EntitySource,
		SourceID:            t.EntitySourceID,
		SourceURL:           t.SourceURL,
		Title:               t.Title,
		EntityKind:          t.EntityKind,
		EventType:           t.EventType,
		DedupKey:            t.DedupKey,
		Severity:            t.Severity,
		RelevanceReason:     t.RelevanceReason,
		ScoringStatus:       t.ScoringStatus,
		CreatedAt:           t.CreatedAt.Format(time.RFC3339),
		Status:              t.Status,
		PriorityScore:       t.PriorityScore,
		AutonomySuitability: t.AutonomySuitability,
		AISummary:           t.AISummary,
		PriorityReasoning:   t.PriorityReasoning,
		CloseReason:         t.CloseReason,
		SnoozeUntil:         snoozeUntil,
		OpenSubtaskCount:    t.OpenSubtaskCount,
		ClaimedByAgentID:    t.ClaimedByAgentID,
		ClaimedByUserID:     t.ClaimedByUserID,
		TeamID:              teamIDString(t.TeamID),
	}
}

// teamIDString flattens a task's nullable owning team to "" when unresolved
// (team_id NULL) so the response keeps emitting a plain string; the
// json:"team_id,omitempty" tag then omits the field entirely for an unowned
// task rather than sending an empty-string team.
func teamIDString(teamID *string) string {
	if teamID == nil {
		return ""
	}
	return *teamID
}

// taskIDOr404 validates the {id} path value as a UUID and writes the 404 for
// a malformed one. On Postgres tasks.id is the uuid column type, so a
// malformed id would otherwise surface as SQLSTATE 22P02 from the first store
// call → 500. Treating malformed ids as "task not found" keeps the API
// portable across SQLite (id TEXT, no parse error) and Postgres, and matches
// the disclosure rule — a malformed id names nothing the caller may learn
// about.
func taskIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "task")
		return "", false
	}
	return id, true
}

// taskLaneRequest is the lane half of a task query: the filters that decide
// which rows a surface is ABOUT, as against the ones a reader then applies
// inside it. Both of the tasks resource's reads take it — the list renders
// the lane's rows, its facet counts them by event type — from one struct and
// one validator, so a rule can't hold on one door and not its sibling.
//
// Every field is optional. The zero lane is "every task I can see that isn't
// sleeping", and nothing about it is implicit: a caller that wants only the
// pickable queue says so, and one that wants the last week of closed work
// passes the window it means.
type taskLaneRequest struct {
	// Statuses selects the lanes. Empty = all. Validated against
	// db.TaskListStatuses; an unknown value is a client fault, not an
	// empty page.
	Statuses []string `json:"statuses"`
	// TeamIDs is the per-page multi-team view scope. Empty = the union of
	// the viewer's teams (the RLS-scoped default). Well-formedness is
	// checked strictly: a corrupt filter must not silently widen back to
	// the union.
	TeamIDs []string `json:"team_ids"`
	// OnlyUnclaimed keeps just the rows nobody has taken.
	OnlyUnclaimed bool `json:"only_unclaimed"`
	// IncludeSnoozed keeps rows still inside their snooze window.
	IncludeSnoozed bool `json:"include_snoozed"`
	// ClosedSince (RFC3339) windows done/dismissed rows by closed_at.
	// Absent = no window. The board asks for the seven days it wants to
	// render; the server no longer applies one behind the caller's back.
	ClosedSince string `json:"closed_since"`
	// CreatedSince (RFC3339) keeps rows created at or after it, whatever
	// their status — combined with a count-only page it is the "tasks over
	// the last N days" figure. Absent = no window.
	CreatedSince string `json:"created_since"`
	// Sources narrows to tasks whose entity came from these sources.
	// Validated against the event catalog's source vocabulary
	// (domain.EventSources); an unknown value is a client fault, not an
	// empty page. Empty = all sources.
	Sources []string `json:"sources"`
}

// taskListRequest is the body of POST /api/tasks/list: the lane, the reader's
// own narrowing of it, and the shared paging fields.
type taskListRequest struct {
	taskLaneRequest

	// CreatedBefore (RFC3339) is CreatedSince's other half: rows created at
	// or before it. The pair is how a lane asks for a window rather than a
	// ray, and either end may stand alone. It sits with the reader's own
	// narrowing rather than in the lane: it is the moving end of the window
	// a column's filter popover offers, so a facet computed under it would
	// describe the reader's slice instead of the column.
	CreatedBefore string `json:"created_before"`
	// EventTypes narrows to these stations. Validated against the catalog
	// (domain.EventTypeIDs) on the same terms as Sources. Empty = all.
	EventTypes []string `json:"event_types"`
	// Search is a case-insensitive substring the row must carry in its
	// title, source id, ai_summary or event type. Trimmed; empty after the
	// trim is absent. It runs here rather than in the client because a lane
	// holds one page: a match on an unfetched page is invisible to a
	// client-side filter, and the tail's total would answer a different
	// query from the items above it.
	Search string `json:"search"`
	// SortKey reorders the lane (db.TaskListSortKeys). Absent asks for the
	// store's default order — there is deliberately no "default" value to
	// send, since a name for the absent case is a second spelling of it.
	SortKey string `json:"sort_key"`
	// SortDir is SortKey's direction. Absent alongside a key means desc.
	// Alone it is refused rather than ignored: the default order has no
	// direction, so a lone sort_dir is a caller expecting something the
	// answer will not show.
	SortDir string `json:"sort_dir"`

	httpx.PageRequest
}

// taskLaneKeys is the part of a resolved lane the page fingerprint needs and
// the store filter cannot supply: the fingerprint is taken over text, and a
// *time.Time has no canonical spelling of its own.
type taskLaneKeys struct {
	ClosedSince  string
	CreatedSince string
}

// resolveTaskLane validates the lane filters and renders them as the store
// filter both task reads take. Faults land on v — every failing field, not
// the first — so a caller sees the whole bad body at once.
func resolveTaskLane(v *httpx.Validation, req taskLaneRequest) (db.TaskListFilter, taskLaneKeys) {
	f := db.TaskListFilter{
		Statuses:       canonicalStrings(req.Statuses),
		TeamIDs:        canonicalStrings(req.TeamIDs),
		OnlyUnclaimed:  req.OnlyUnclaimed,
		IncludeSnoozed: req.IncludeSnoozed,
		Sources:        canonicalStrings(req.Sources),
	}
	for _, st := range f.Statuses {
		if !slices.Contains(db.TaskListStatuses, st) {
			v.Invalid("statuses", fmt.Sprintf("unknown status %q; must be one of: %s",
				st, strings.Join(db.TaskListStatuses, ", ")))
		}
	}
	for _, id := range f.TeamIDs {
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("team_ids", fmt.Sprintf("team id %q is not a valid team id", id))
		}
	}
	validSources := domain.EventSources()
	for _, src := range f.Sources {
		if !slices.Contains(validSources, src) {
			v.Invalid("sources", fmt.Sprintf("unknown source %q; must be one of: %s",
				src, strings.Join(validSources, ", ")))
		}
	}
	var keys taskLaneKeys
	f.ClosedSince, keys.ClosedSince = parseTaskWindow(v, "closed_since", req.ClosedSince)
	f.CreatedSince, keys.CreatedSince = parseTaskWindow(v, "created_since", req.CreatedSince)
	return f, keys
}

// parseTaskWindow parses one RFC3339 bound of a task query's time window,
// returning the instant and the canonical text the page fingerprint is taken
// over. An absent bound is (nil, "") — no window — and a malformed one names
// its own field rather than the pair's.
func parseTaskWindow(v *httpx.Validation, field, raw string) (*time.Time, string) {
	if raw == "" {
		return nil, ""
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		v.Invalid(field, field+" must be an RFC3339 timestamp")
		return nil, ""
	}
	ts = ts.UTC()
	return &ts, ts.Format(time.RFC3339Nano)
}

// taskListFilterKey is the canonicalized form of a taskListRequest's filters —
// sorted, deduped, and normalized — that the page token is fingerprinted
// against. Two requests that mean the same query fingerprint identically, and
// a token minted for one filter set is refused for another.
type taskListFilterKey struct {
	Statuses       []string `json:"statuses"`
	TeamIDs        []string `json:"team_ids"`
	OnlyUnclaimed  bool     `json:"only_unclaimed"`
	IncludeSnoozed bool     `json:"include_snoozed"`
	ClosedSince    string   `json:"closed_since"`
	CreatedSince   string   `json:"created_since"`
	CreatedBefore  string   `json:"created_before"`
	Sources        []string `json:"sources"`
	EventTypes     []string `json:"event_types"`
	Search         string   `json:"search"`
	// The sort is part of the key for the same reason the filters are: an
	// offset addresses a position in an ordering, so a token minted under
	// one sort names different rows under another.
	SortKey string `json:"sort_key"`
	SortDir string `json:"sort_dir"`
}

// maxTaskSearchRunes caps the search needle. A needle longer than this is a
// paste, not a search, and the cap is measured in runes so a multi-byte one
// isn't refused for being multi-byte.
const maxTaskSearchRunes = 200

// taskCreateRequest is the body of POST /api/tasks: the station a task is
// wanted at, named the way the dedup index names it. dedup_key may be empty —
// it is only non-empty for the open-set discriminators (a label name, a status
// name) that get a task per value.
//
// team_id is the acting team the task is created under. Required in the UI when
// the caller belongs to ≥2 teams; empty falls back to the sole team.
//
// There is deliberately no blueprint_id: firing a run is POST
// /api/tasks/{id}/delegate, a second call on the row this one resolved.
type taskCreateRequest struct {
	EntityID  string `json:"entity_id"`
	EventType string `json:"event_type"`
	DedupKey  string `json:"dedup_key"`
	TeamID    string `json:"team_id"`
}

// handleTaskCreate resolves the task at (entity_id, event_type, dedup_key) for
// the acting team, creating it if the station has none — the write behind the
// Factory's drag-to-delegate gesture, and the way any caller says "this entity
// needs attention here" without waiting for a poller to say it.
//
// Find-or-create rather than create: the partial unique index
// idx_tasks_active_entity_event_dedup makes concurrent calls resolve to the
// same row, so the gesture is idempotent by construction and two users dropping
// the same chip get one task. 201 when this call minted it, 200 when it found
// one — the only thing the two answers differ in, since both hand back the same
// resource a GET /api/tasks/{id} would.
//
// Three refusals, all before any write:
//   - the entity must exist (404) and be active (409) — the factory snapshot's
//     60s soft-close grace lets a chip ride the final animation hop after its
//     entity flipped to merged/closed, and a task synthesized there would run
//     to completion against a closed PR with no close-cascade to clean it up;
//   - the (entity, event_type, dedup_key) triple must resolve a real event
//     (422). tasks.primary_event_id is NOT NULL: a task cannot exist without an
//     anchor, and if nothing matches, the entity isn't at this station;
//   - a viewer is rejected on the acting team (403) before anything is
//     synthesized — without the explicit gate the tasks_insert RLS WITH CHECK
//     would fail the write as a generic 500 rather than a role-named refusal.
//
// POST /api/tasks
func (s *Server) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var req taskCreateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.EntityID == "" {
		v.Missing("entity_id")
	}
	if req.EventType == "" {
		v.Missing("event_type")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Resolve the acting team read-only (no last-acting stamp — we may be about
	// to 403) and gate on it. A bad team pick 400s via the selection-error
	// mapping, same as the write path below.
	var actingTeam string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "tasks", err)
		return
	}
	if !s.az.RequireTeamWrite(w, r, orgID, userID, actingTeam) {
		return
	}

	var entity *domain.Entity
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		entity, e = tx.Entities.Get(r.Context(), orgID, req.EntityID)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if entity == nil {
		notFound(w, "entity")
		return
	}
	if entity.State != "active" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonAlreadyTerminal,
			Message: "entity is closed; cannot create a task on it",
		})
		return
	}

	// Anchor on the most recent event matching all three of (entity_id,
	// event_type, dedup_key). The dedup_key filter is pushed into the SQL —
	// picking the latest event by type alone and rejecting a mismatch would 422
	// every time a sibling discriminator (label_added "help wanted") fired more
	// recently than the requested one (label_added "bug").
	var primaryEvent *domain.Event
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		primaryEvent, e = tx.Events.LatestForEntityTypeAndDedupKey(r.Context(), orgID, req.EntityID, req.EventType, req.DedupKey)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if primaryEvent == nil {
		// The body parsed fine; the triple it names doesn't exist — a semantic
		// fault in what was referenced, so 422 rather than 400.
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "no matching event for entity at this station",
		})
		return
	}

	schema, schemaOK := events.Get(req.EventType)

	var (
		task    *domain.Task
		created bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Resolve the acting team FIRST, so the priority scan below can limit
		// itself to that team's rules. Scanning before resolving let a sibling
		// team's high-priority rule inflate a task created for a different team.
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}

		priority, e := teamDefaultPriority(r.Context(), tx, orgID, teamID, req.EventType, primaryEvent, schema, schemaOK)
		if e != nil {
			return e
		}

		task, created, e = tx.Tasks.FindOrCreate(r.Context(), orgID, teamID, req.EntityID, req.EventType, req.DedupKey, primaryEvent.ID, priority)
		if e != nil {
			return e
		}
		// Mirror the router's audit linkage: a brand-new task gets a task_events
		// row linking it to the event it was anchored on, kind="spawned", so a
		// timeline reading task_events sees one shape regardless of which path
		// created the task. Non-fatal — an audit gap beats failing a write that
		// already landed.
		//
		// No kind="bumped" on the find branch: nothing new landed, the caller
		// just named a station that already has a task.
		if created {
			if recErr := tx.Tasks.RecordEvent(r.Context(), orgID, task.ID, primaryEvent.ID, "spawned"); recErr != nil {
				tasksLog.Warn("failed to record spawned task_event", "task", task.ID, "error", recErr)
			}
		}
		return nil
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "tasks", err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, taskToJSON(*task))
}

// teamDefaultPriority is the priority a task gets at creation when no scorer has
// run yet: the highest default among the enabled rules that both belong to
// teamID and match this event, floored at 0.5. It mirrors the predicate-match
// filter in internal/routing so a task minted by hand lands at the same priority
// the router would have given one minted by a poll.
//
// Only rules belonging to the acting team (or org-visible system rules with no
// team) count: a rule owned by another team must not lift the priority of a task
// created for this one. The predicate gate stays for the same reason in the other
// direction — iterating every enabled rule would inflate priority whenever a
// high-priority rule's scope_predicate doesn't match this event's metadata.
// Empty predJSON always matches, per the events package contract.
func teamDefaultPriority(ctx context.Context, tx db.TxStores, orgID, teamID, eventType string, primaryEvent *domain.Event, schema events.EventSchema, schemaOK bool) (float64, error) {
	priority := 0.5
	handlers, err := tx.EventHandlers.GetEnabledForEvent(ctx, orgID, eventType)
	if err != nil {
		return 0, err
	}
	for _, h := range handlers {
		if h.Kind != domain.EventHandlerKindRule || h.DefaultPriority == nil {
			// Trigger rows have no DefaultPriority; skip.
			continue
		}
		// Multi-mode system rules are seeded per-team (visibility='team', real
		// team_id), so this counts the acting team's own shipped rules and
		// excludes siblings'.
		if h.TeamID != "" && h.TeamID != teamID {
			continue
		}
		if !schemaOK {
			// No registered schema → the predicate can't be evaluated. Mirrors
			// matchPredicate's quietly-permissive behavior: skip the rule and
			// fall back to the floor.
			continue
		}
		predJSON := ""
		if h.ScopePredicateJSON != nil {
			predJSON = *h.ScopePredicateJSON
		}
		matched, merr := schema.Match(predJSON, primaryEvent.MetadataJSON)
		if merr != nil {
			tasksLog.Warn("event_handler predicate error, skipping", "event_handler", h.ID, "error", merr)
			continue
		}
		if matched && *h.DefaultPriority > priority {
			priority = *h.DefaultPriority
		}
	}
	return priority, nil
}

// handleTaskList is the tasks resource's list read — every task-list surface
// (the triage deck, each board column) is a filter set over this one route.
//
// It is a POST because the filters are a body, not a query string: repeated
// ?status=/?team_id= params were how the old GET pair drifted into two
// spellings of the same read with different hidden defaults. A body-carrying
// read registers through apiMutating like any other POST so the CSRF
// same-origin check applies symmetrically — the method is what a browser
// preflights on, not the intent. The read itself has no side effects.
//
// It is the one list route that pages by keyset rather than offset, because it
// is the one whose rows move under the reader: a run finishes and its card
// leaves In Progress, a card is dragged, a poll mints a task at the head of
// the queue. An offset page 2 of a lane that lost a row above the cut skips
// one, and the board fetches on scroll with no way to re-ask, so a skipped row
// is simply never seen. The token stays opaque either way, so nothing a client
// reads or writes changes shape.
//
// It accepts keyset tokens ONLY. An offset-form token — one this route minted
// before the conversion — would still page, because the stores keep offset
// paging for the routes that use it, and that is exactly why it is refused
// (400 INVALID_PARAM, restart from the first page): an input that makes this
// route page the way it no longer pages is the bug still reachable by request.
// Nothing else holds such a token for long — the SPA ships inside this binary,
// so there is no version skew to bridge — and a headless caller mid-walk gets
// correct paging one request sooner by starting over.
func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req taskListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	var v httpx.Validation
	filter, laneKeys := resolveTaskLane(&v, req.taskLaneRequest)
	var createdBeforeKey string
	filter.CreatedBefore, createdBeforeKey = parseTaskWindow(&v, "created_before", req.CreatedBefore)
	filter.EventTypes = canonicalStrings(req.EventTypes)
	validEventTypes := domain.EventTypeIDs()
	for _, et := range filter.EventTypes {
		if !slices.Contains(validEventTypes, et) {
			// The catalog is too long to spell into an error message, and it
			// is already a route: name the offending value and say where the
			// vocabulary lives.
			v.Invalid("event_types", fmt.Sprintf("unknown event type %q; see GET /api/event-types for the catalog", et))
		}
	}
	// Trimmed here rather than in the store so the fingerprint, the predicate
	// and the cap all see the same needle — " ci " and "ci" are one query.
	filter.Search = strings.TrimSpace(req.Search)
	if utf8.RuneCountInString(filter.Search) > maxTaskSearchRunes {
		v.Invalid("search", fmt.Sprintf("search must be at most %d characters", maxTaskSearchRunes))
	}
	filter.SortKey, filter.SortDir = req.SortKey, req.SortDir
	if filter.SortKey != "" && !slices.Contains(db.TaskListSortKeys, filter.SortKey) {
		v.Invalid("sort_key", fmt.Sprintf("unknown sort_key %q; must be one of: %s",
			filter.SortKey, strings.Join(db.TaskListSortKeys, ", ")))
	}
	if filter.SortDir != "" {
		if !slices.Contains(db.TaskListSortDirs, filter.SortDir) {
			v.Invalid("sort_dir", fmt.Sprintf("unknown sort_dir %q; must be one of: %s",
				filter.SortDir, strings.Join(db.TaskListSortDirs, ", ")))
		}
		if filter.SortKey == "" {
			v.Invalid("sort_dir", "sort_dir requires sort_key")
		}
	} else if filter.SortKey != "" {
		// Newest / Z-A first, which is what the board's controls default to.
		filter.SortDir = db.TaskSortDirDesc
	}
	page := httpx.ResolveKeysetPage(&v, req.PageRequest, httpx.FilterFingerprint(taskListFilterKey{
		Statuses:       filter.Statuses,
		TeamIDs:        filter.TeamIDs,
		OnlyUnclaimed:  filter.OnlyUnclaimed,
		IncludeSnoozed: filter.IncludeSnoozed,
		ClosedSince:    laneKeys.ClosedSince,
		CreatedSince:   laneKeys.CreatedSince,
		CreatedBefore:  createdBeforeKey,
		Sources:        filter.Sources,
		EventTypes:     filter.EventTypes,
		// Case-folded, because the predicate is: two spellings of one needle
		// match the same rows, so they must fingerprint as the same query
		// rather than cost the caller its token.
		Search:  strings.ToLower(filter.Search),
		SortKey: filter.SortKey,
		SortDir: filter.SortDir,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// One row past the window, so the answer to "is there a page after this
	// one" is a row we either got or didn't. A keyset page cannot derive it
	// from the total the way an offset page does, because it doesn't know its
	// own position in the result set — and a lane that loses a row while it is
	// being read is exactly why it must not pretend to.
	opts := db.ListOpts{Limit: page.Limit, Offset: page.Offset, After: page.After, CountOnly: page.CountOnly}
	if opts.Limit > 0 {
		opts.Limit++
	}

	var tasks []domain.Task
	var total int
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		tasks, total, e = tx.Tasks.List(r.Context(), orgID, filter, opts)
		return e
	}); err != nil {
		if errors.Is(err, db.ErrBadPageCursor) {
			// A cursor that doesn't fit the order it names: not a token this
			// build minted, so it is a bad page_token like any other
			// unreadable one rather than a server fault.
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidParam,
				Message: "page_token does not address this ordering; restart from the first page",
				Field:   "page_token",
			})
			return
		}
		internalError(w, "tasks", err)
		return
	}

	var nextKey []string
	if page.Limit > 0 && len(tasks) > page.Limit {
		tasks = tasks[:page.Limit]
		nextKey = db.TaskSortKey(filter, tasks[len(tasks)-1])
	}
	items := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		items[i] = taskToJSON(t)
	}
	httpx.WriteListKeyset(w, page, items, total, nextKey)
}

// taskFacetsRequest is the body of POST /api/tasks/facets: the lane, and
// nothing a reader narrows it with.
//
// The refused fields are declared here rather than left to strict decode's
// unknown-field arm, because "unknown field" would be the wrong answer: each
// is a real field of the sibling list, so a caller sending one has misread
// what this route answers rather than mistyped a name. They are
// json.RawMessage so PRESENCE is what's detected — `"search": ""` is still a
// caller narrowing a lane whose whole point is to be unnarrowed — and the
// paging pair is spelled out here instead of embedding httpx.PageRequest,
// which would accept them.
type taskFacetsRequest struct {
	taskLaneRequest

	EventTypes    json.RawMessage `json:"event_types"`
	Search        json.RawMessage `json:"search"`
	SortKey       json.RawMessage `json:"sort_key"`
	SortDir       json.RawMessage `json:"sort_dir"`
	CreatedBefore json.RawMessage `json:"created_before"`
	PageSize      json.RawMessage `json:"page_size"`
	PageToken     json.RawMessage `json:"page_token"`
}

// taskFacet is one event type present in a lane and how many of its tasks sit
// there. `value` rather than `event_type` because the shape is the resource's
// facet shape: a second cut (sources) is a sibling key of the response, not a
// second shape.
type taskFacet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// taskFacetsResponse is the fixed schema of named cuts a synthetic read
// answers with. One cut today; a `sources` cut lands beside this key without
// a wire change, and never as a `group_by=` parameter that would reshape the
// response.
type taskFacetsResponse struct {
	EventTypes []taskFacet `json:"event_types"`
}

// handleTaskFacets is the tasks list's synthetic sibling: the event types
// present in a lane and how many rows carry each — the set a lane's filter
// chips are drawn from.
//
// It is a node beside the list rather than a field on it because the answer
// is numbers about rows rather than rows, and it takes the lane's filters
// without the reader's own: the chips exist to show which types the lane
// holds INCLUDING the ones the reader just filtered out, so a facet narrowed
// by `event_types` would answer about itself. A client cannot derive the set
// from the list either — it holds one page of a filtered query, and a lane
// whose first page is all one type would offer no chip for the twenty rows of
// another sitting on page two.
//
// No paging and no total_count: the grouping is over a closed vocabulary, so
// the answer is bounded by the catalog rather than by a window. Authorization
// is the list's — RLS scopes the rows and team_ids narrows within that — and
// it registers through apiMutating for the same reason the list does: it is a
// POST, and CSRF follows the method rather than the intent.
//
// POST /api/tasks/facets
func (s *Server) handleTaskFacets(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req taskFacetsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	var v httpx.Validation
	const narrows = "%s narrows a lane; the facet answers for the whole lane, including the values a reader filtered out"
	const pages = "%s is not accepted: the facet is one bounded answer over the event-type catalog, not a page"
	for _, refused := range []struct {
		field  string
		sent   json.RawMessage
		reason string
	}{
		{"event_types", req.EventTypes, narrows},
		{"search", req.Search, narrows},
		{"sort_key", req.SortKey, narrows},
		{"sort_dir", req.SortDir, narrows},
		{"created_before", req.CreatedBefore, narrows},
		{"page_size", req.PageSize, pages},
		{"page_token", req.PageToken, pages},
	} {
		if refused.sent != nil {
			v.Invalid(refused.field, fmt.Sprintf(refused.reason, refused.field))
		}
	}
	filter, _ := resolveTaskLane(&v, req.taskLaneRequest)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var facets []db.Facet
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		facets, e = tx.Tasks.FacetEventTypes(r.Context(), orgID, filter)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	out := taskFacetsResponse{EventTypes: make([]taskFacet, len(facets))}
	for i, f := range facets {
		out.EventTypes[i] = taskFacet{Value: f.Value, Count: f.Count}
	}
	writeJSON(w, http.StatusOK, out)
}

// canonicalStrings sorts and dedups a filter list so the same query always
// produces the same page-token fingerprint however the client ordered it, and
// so a repeated value can't multiply an IN-list. Nil in, nil out — an absent
// filter and an empty one mean the same thing (no narrowing) and must
// fingerprint the same.
func canonicalStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	return out
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	writeJSON(w, http.StatusOK, taskToJSON(*task))
}

// Task status vocabulary, split by who owns each write. Terminal statuses
// close a task; the stage statuses are the user's own progress markers.
const (
	taskStatusQueued     = "queued"
	taskStatusInProgress = "in_progress"
	taskStatusInReview   = "in_review"
	taskStatusSnoozed    = "snoozed"
	taskStatusDone       = "done"
	taskStatusDismissed  = "dismissed"
)

// The swipe_events action vocabulary. These strings are stored data — every
// row written since the swipe deck shipped carries one of them, and the
// analytics views group on them — so a route may change which gesture writes
// which string, but never the strings themselves.
const (
	swipeActionClaim    = "claim"
	swipeActionDismiss  = "dismiss"
	swipeActionSnooze   = "snooze"
	swipeActionDelegate = "delegate"
	swipeActionComplete = "complete"
	swipeActionReassign = "reassign"
)

// isTerminalTaskStatus reports whether a task is closed. A closed task takes
// no further mutation except the two routes that re-open it (requeue / undo),
// so every other handler refuses on this predicate rather than writing over
// closed_at / close_reason.
func isTerminalTaskStatus(status string) bool {
	return status == taskStatusDone || status == taskStatusDismissed
}

// writeTaskTerminal is the shared refusal for a mutation aimed at a closed
// task. One reason and one status across every task route, so a client can
// branch on ALREADY_TERMINAL without knowing which verb it called.
func writeTaskTerminal(w http.ResponseWriter, verb string) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonAlreadyTerminal,
		Message: "task is closed; " + verb + " transitions aren't allowed past close",
	})
}

// taskPatchRequest is the body of PATCH /api/tasks/{id}. json.RawMessage
// rather than *string on snooze_until so an absent field (keep the wake time)
// and an explicit null (clear it) stay distinguishable — the repos PATCH is
// the reference for the convention.
type taskPatchRequest struct {
	Status      json.RawMessage `json:"status,omitempty"`
	SnoozeUntil json.RawMessage `json:"snooze_until,omitempty"`
	// HesitationMs is the dwell time before the gesture, recorded on the
	// swipe_events row the dismiss / complete / snooze arms write. The stage
	// arms (in_progress / in_review) write no audit row, so carrying it there
	// would be a field the server accepts and silently drops.
	HesitationMs int `json:"hesitation_ms,omitempty"`
}

// handleTaskPatch is the task's field-write path: the lifecycle axis (done,
// dismissed, the in_progress → in_review stages) and the wake time. Effects
// that a field write can't express keep their own verb routes — claim and
// delegate reach external systems and spawn runs, requeue and undo tear down
// artifacts and reverse an audit row.
//
// Guards apply in two rounds, and the order is deliberate: the body is
// validated first (400 for shape, 422 for a value out of range), and only a
// body that means something is measured against the task's state. So a
// malformed request answers for its own shape even when the task also happens
// to be closed — the task's state is not what is wrong with it. A body naming
// no field is a 400 too: it wrote nothing, and "updated" is the one answer a
// client can't tell from a real write.
//
// Then the state round: a closed task refuses everything (409
// ALREADY_TERMINAL — requeue and undo are how a task re-opens), and the stage
// statuses require the caller's own active claim (403). Neither round is the
// last word on safety; the store carries its own predicates, because the row
// can change between this handler's read and its write.
//
// PATCH /api/tasks/{id}
func (s *Server) handleTaskPatch(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	var req taskPatchRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	// Two validations because the two fault classes carry different
	// statuses: shape faults are 400 ("you didn't say it right") and range
	// faults 422 ("you said it right and the value can't be one"). Each
	// flushes as a blanket status over everything it accumulated, and the
	// shape pass goes first — a value can't be out of range until it parses.
	var shape, semantic httpx.Validation
	if !httpx.PatchNamed(req.Status, req.SnoozeUntil) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide status or snooze_until (null clears the snooze)",
		})
		return
	}
	// One axis per request. A body carrying both says two contradictory
	// things about where the task lands — snoozing moves it to 'snoozed', and
	// status names something else — and picking a winner would make the other
	// field a silently-dropped one.
	if req.Status != nil && req.SnoozeUntil != nil {
		shape.Invalid("status", "status and snooze_until can't be set in one request: snoozing moves the task to snoozed by itself")
	}

	status, statusState := httpx.PatchString(&shape, req.Status, "status")
	if statusState == httpx.PatchClear {
		shape.Invalid("status", "status can't be cleared: a task always has one")
	}
	if statusState == httpx.PatchSet && !slices.Contains(patchableTaskStatuses, status) {
		shape.Invalid("status", "status must be one of: "+strings.Join(patchableTaskStatuses, ", "))
	}
	snoozeUntil, snoozeState := parseSnoozeUntilPatch(&shape, &semantic, req.SnoozeUntil)
	validateHesitation(&shape, req.HesitationMs)
	// hesitation_ms is the dwell time behind a card gesture, and only the
	// arms that write a swipe_events row have somewhere to put it. Rejecting
	// it elsewhere beats accepting a number and dropping it.
	if req.HesitationMs != 0 && !patchWritesAudit(status, statusState, snoozeState) {
		shape.Invalid("hesitation_ms", "hesitation_ms applies to the dismissed / done / snooze gestures only")
	}
	if shape.Flush(w, http.StatusBadRequest) {
		return
	}
	if semantic.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	// Every arm here is a team-scoped write — viewers can't.
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	// A closed task is closed: no arm of this route may write over its
	// closed_at / close_reason, snooze it, or move it back to a stage.
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "status")
		return
	}

	switch {
	case snoozeState == httpx.PatchSet:
		if !s.patchSnooze(w, r, orgID, userID, id, snoozeUntil, req.HesitationMs) {
			return
		}
	case snoozeState == httpx.PatchClear:
		if !s.patchWake(w, r, orgID, userID, id) {
			return
		}
	case status == taskStatusInProgress || status == taskStatusInReview:
		if !s.patchStage(w, r, orgID, userID, id, task, status) {
			return
		}
	default:
		if !s.patchClose(w, r, orgID, userID, id, status, req.HesitationMs) {
			return
		}
	}

	s.writeTaskResource(w, r, orgID, userID, id)
}

// writeTaskResource serves a task's current row in the shape the reads serve
// it — not a status stub, and not the request body (or a caller-computed
// guess) echoed back. A task write hands back at most the tasks row's own
// columns (see the returned-row shape note on db.TaskStore), never the
// entity-joined display fields this resource carries, so every
// write-then-respond handler goes through this point read instead — under
// the requester's RLS, so the response also respects their visibility.
// Writes the JSON body and returns true on success; writes its own error
// response and returns false on a store error or a task that vanished
// between the write and this read.
func (s *Server) writeTaskResource(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var updated *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Tasks.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return false
	}
	if updated == nil {
		notFound(w, "task")
		return false
	}
	writeJSON(w, http.StatusOK, taskToJSON(*updated))
	return true
}

// writeTaskResourceSystem is writeTaskResource on the ADMIN pool
// (Tasks.GetSystem), for the one write whose success can remove the row
// from the actor's own sight: a reassign handoff consolidates team_id to
// the target's team, which an overriding admin need not be a member of, so
// the RLS-scoped read would answer 404 for a mutation that landed. The read
// carries the same authority the mutation it reports on already used —
// reassign's permission arms are decided in Go and the write runs on the
// admin pool for exactly this reason (see reassignClaim) — and it discloses
// nothing new: reassignClaim's entry read proved the actor could see this
// task under their own RLS within this same request.
func (s *Server) writeTaskResourceSystem(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var updated *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Tasks.GetSystem(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return false
	}
	if updated == nil {
		notFound(w, "task")
		return false
	}
	writeJSON(w, http.StatusOK, taskToJSON(*updated))
	return true
}

// patchableTaskStatuses is the set PATCH accepts. 'queued' isn't among them:
// a task returns to the queue through requeue / undo, which also release the
// claim and tear down whatever the agent left behind — a bare status write
// would strand both. 'snoozed' isn't either: it is what setting snooze_until
// means, and accepting it as a status would be a second way to say it, one
// that can't carry the wake time the row needs.
var patchableTaskStatuses = []string{taskStatusInProgress, taskStatusInReview, taskStatusDone, taskStatusDismissed}

// patchWritesAudit reports whether the PATCH body describes a gesture that
// lands a swipe_events row — the dismiss / complete / snooze arms. The stage
// advances and the wake are field writes with no gesture behind them.
func patchWritesAudit(status string, statusState, snoozeState httpx.PatchState) bool {
	if snoozeState == httpx.PatchSet {
		return true
	}
	return statusState == httpx.PatchSet && (status == taskStatusDone || status == taskStatusDismissed)
}

// patchSnooze parks a task until the given wake time. Snooze is queue-only
// ("snoozed ↔ both claim cols NULL") and never applies to a closed task, so
// the store's atomic UPDATE carries both predicates and the caller gets a 409
// telling them to requeue first.
func (s *Server) patchSnooze(w http.ResponseWriter, r *http.Request, orgID, userID, id string, until time.Time, hesitationMs int) bool {
	// errSnoozeRefusedSentinel rolls the outer tx back when SnoozeTask returns
	// (false, nil): the swipes store relies on a tx-level rollback to discard
	// the audit row it inserted before the claim-guard UPDATE refused, and a
	// flat (no-error) return here would commit that audit row.
	errSnoozeRefusedSentinel := errors.New("snooze refused")
	var snoozed bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		snoozed, e = tx.Swipes.SnoozeTask(r.Context(), orgID, id, until, hesitationMs)
		if e != nil {
			return e
		}
		if !snoozed {
			return errSnoozeRefusedSentinel
		}
		return nil
	}); err != nil && !errors.Is(err, errSnoozeRefusedSentinel) {
		internalError(w, "tasks", err)
		return false
	}
	if !snoozed {
		// Either guard in the store's predicate: the task is claimed, or it
		// closed between the pre-read above and this write.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "can't snooze this task; snoozing needs an open, unclaimed task — requeue or complete it first",
		})
		return false
	}
	s.broadcastTaskStatus(orgID, id, taskStatusSnoozed)
	return true
}

// patchWake clears a wake time early, returning the task to the queue. The
// store guards on status='snoozed' so clearing a wake time a task doesn't have
// is a 409 rather than a silent drag out of whatever lane it was in.
func (s *Server) patchWake(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var woke bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		woke, e = tx.Swipes.ClearSnooze(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return false
	}
	if !woke {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task isn't snoozed, so there's no wake time to clear",
			Field:   "snooze_until",
		})
		return false
	}
	s.broadcastTaskStatus(orgID, id, taskStatusQueued)
	return true
}

// patchStage moves a task between the user's own progress markers — "I'm
// working on this now" and "I've submitted this for review". Both require the
// caller to hold the user claim: a bot-claimed task advances through the
// spawner, and an unclaimed one has nobody whose progress this would be.
func (s *Server) patchStage(w http.ResponseWriter, r *http.Request, orgID, userID, id string, task *domain.Task, status string) bool {
	if task.ClaimedByUserID != userID {
		forbidden(w, "only the user holding this task's claim can move it between in_progress and in_review")
		return false
	}
	var advanced bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		advanced, e = tx.Tasks.AdvanceStatusForUser(r.Context(), orgID, id, userID, status)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return false
	}
	if !advanced {
		// The pre-read said the caller holds the claim and the task isn't
		// closed, so a refusal here means something moved underneath us.
		// Losing is terminal for this attempt — the client refetches.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task moved since it was read; refetch and retry",
		})
		return false
	}
	s.broadcastTaskStatus(orgID, id, status)
	return true
}

// patchClose is the terminal arm: dismissed (walked away) or done (resolved).
// The write and its audit row land in one store call, and that call carries the
// terminal predicate itself — the pre-read in the handler answers the common
// case early, but only the predicate rules out a close landing between the two,
// which is what a second close rewriting closed_at / close_reason would be.
func (s *Server) patchClose(w http.ResponseWriter, r *http.Request, orgID, userID, id, status string, hesitationMs int) bool {
	action := swipeActionDismiss
	if status == taskStatusDone {
		action = swipeActionComplete
	}
	var newStatus string
	var closed bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newStatus, closed, e = tx.Swipes.RecordSwipe(r.Context(), orgID, id, action, hesitationMs, nil)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return false
	}
	if !closed {
		// Someone else closed it in the window since the pre-read. Same
		// answer the pre-read would have given a moment later, and nothing
		// was written.
		writeTaskTerminal(w, "status")
		return false
	}
	// Closing a task takes it off the agent's hands: stop any in-flight run
	// and resolve every unresolved artifact it holds.
	s.teardownTaskConversations(context.WithoutCancel(r.Context()), orgID, userID, id, delegate.StopCauseTaskDispositioned)
	s.broadcastTaskStatus(orgID, id, newStatus)
	return true
}

// broadcastTaskStatus nudges peer sessions onto a task's new lane. Without it
// a dismissed / completed / snoozed / advanced task stays where it was on
// other browsers until the next user-driven refresh.
func (s *Server) broadcastTaskStatus(orgID, id, status string) {
	if s.ws == nil {
		return
	}
	s.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": id, "status": status},
	})
}

// handleUndo backs the Cards swipe-toast UX: the user just swiped
// claim/dismiss/delegate/snooze, sees the 5s "Undo" toast (or hits
// Cmd-Z), and we reverse the swipe. This endpoint is specifically
// for undoing a discrete user gesture — it records a swipe_events
// row tagged 'undo' for the swipe analytics, then runs the same
// requeue cleanup that /requeue does.
//
// It reverses the CALLER'S OWN last gesture and nothing else: with no such
// gesture on the task, or with the caller's last one already reversed, the
// store refuses and the route answers 409. Without that the route is a
// force-reset for any task the caller can address, and the deliberate
// version of that gesture — "put this back in the queue" — already exists at
// /requeue, which skips the swipe row. Same finalizer, same observable
// outcome, different audit shape.
//
// POST /api/tasks/{id}/undo
func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	// Undo reverses a swipe — a task mutation a viewer can't make (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	// GetTask up front does double duty: existence check for the
	// 404 response AND loads the row needed for finalizeRequeue's
	// Jira reversal context. Without the explicit nil check
	// UndoLastSwipe would still fail on the swipe_events FK, but
	// we'd surface the SQLite error string as a 500 — leaking
	// implementation detail and confusing legitimate 404 callers.
	var task *domain.Task
	var undone bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if task == nil {
			return nil
		}
		undone, e = tx.Swipes.UndoLastSwipe(r.Context(), orgID, id, userID)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	if !undone {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "no gesture of yours on this task to undo; use requeue to return it to the queue",
		})
		return
	}

	s.finalizeRequeue(r, orgID, userID, id, task)

	s.writeTaskResource(w, r, orgID, userID, id)
}

// handleRequeue is the state-driven counterpart to handleUndo: same
// task-back-to-queue outcome, no swipe_events row. Used by Board's
// drag-to-Queue gesture and the AgentCard's "Return to queue" button
// on a conversation awaiting approval of its artifact. Both of those
// are deliberate state changes, not "reverse my last swipe," so
// audit-logging them as undo events would muddy the swipe-UX
// analytics.
//
// Belt-and-suspenders existence check: GetTask up front catches the
// common bogus-id case and returns 404 with a clean error body;
// RequeueTask's ok-bool catches the race where the task gets
// deleted between the GetTask and the UPDATE. Without the second
// check, that race would surface as a misleading 200/queued
// response for an id that no longer exists.
func (s *Server) handleRequeue(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok {
		return
	}

	// Requeue moves a task back to the queue — a viewer can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	var task *domain.Task
	var requeued bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if task == nil {
			return nil
		}
		requeued, e = tx.Swipes.RequeueTask(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	if !requeued {
		notFound(w, "task")
		return
	}

	s.finalizeRequeue(r, orgID, userID, id, task)

	s.writeTaskResource(w, r, orgID, userID, id)
}

// finalizeRequeue runs the side-effect cleanup that both /undo and
// /requeue need after the task status flips back to queued. Three steps, in
// this order:
//
//   - stop: every run still working the task is stopped and the blueprint
//     behind it cancelled. A requeued task is unclaimed and unowned, so an
//     agent left executing against it keeps writing messages and landing
//     artifacts on a task whose columns say nobody owns it — and the
//     spawner's board-column recompute puts the card straight back out of
//     Queued the moment it re-derives that agent's claim. The stop is what
//     makes returning a live run to the queue safe, which is why the routes
//     above take every status rather than guarding on one.
//
//   - artifact teardown: resolve every unresolved artifact the task's
//     conversations hold (close all draft PRs, dismiss all pending reviews), so
//     a returned-to-queue task leaves no stranded GitHub draft / pending
//     review. It rides inside teardownTaskConversations behind the stop, so it
//     operates on settled conversations; it never flips conversations.status
//     itself.
//
//   - Jira reversal: if the task is Jira-backed and we have a
//     SourceStatus snapshot (recorded at claim time), unassign and
//     transition back. Guarded against external mutations: skip if
//     someone else now owns the ticket, or if the ticket has
//     progressed out of the in-progress rule entirely (done, back to
//     pickup, etc.).
//
// Every step is best-effort and logged-not-failed: the task is
// already queued by the time we get here; failing the response would
// confuse callers about what actually changed.
//
// taskID is taken separately from task because only the Jira reversal needs
// the loaded row, and it nil-guards internally — short-circuiting the whole
// helper on a nil task (db.GetTask transiently failing, or the row deleted
// concurrently) would silently strand the very state this is meant to clean
// up.
//
// orgID + userID are passed rather than re-read from the request so the
// detached cleanup carries the requesting user's identity without rereading a
// (possibly nil-claimed) context post-cancel.
func (s *Server) finalizeRequeue(r *http.Request, orgID, userID, taskID string, task *domain.Task) {
	// One context for the whole cleanup, and it outlives the request: the user
	// already committed to requeueing via the surrounding /undo or /requeue
	// handler, so bailing on browser close would leave a live agent running
	// against a queued task. WithoutCancel inherits the request's values
	// (claims among them) while breaking the cancel chain.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.teardownTaskConversations(cleanupCtx, orgID, userID, taskID, delegate.StopCauseTaskRequeued)
	s.revertJiraStateIfApplicable(cleanupCtx, orgID, userID, task)
	// Requeue clears both claim cols and flips status to
	// 'queued'. Peer Board sessions need a task_updated event to
	// pull the card back into the Queued column; without this they
	// keep showing the stale claim/status until the next refresh.
	// Nothing above broadcasts a task-level change — the stop's events are
	// per-conversation and a resolve is decoupled from conversation lifecycle
	// — so this is the sole board-update signal for a requeue.
	if s.ws != nil {
		s.ws.Broadcast(websocket.Event{
			Type:  "task_updated",
			OrgID: orgID,
			Data:  map[string]any{"task_id": taskID, "status": "queued"},
		})
	}
}

// teardownTaskArtifacts is the task-level "force-resolve-all" gesture: the user
// dragged a card to Done / dismissed it / claimed it / returned it to the queue
// while it still had unresolved artifacts (draft PRs, pending reviews — same or
// different repos). It resolves EVERY unresolved artifact the task's conversations hold so
// nothing strands: each draft PR is closed on GitHub + flipped to closed; each
// pending review (finalized or not) has its GitHub pending review deleted +
// flipped to dismissed. Pushed branches are kept (retention is separate).
//
// This never flips conversations.status. A live conversation is stopped by
// teardownTaskConversations one step ahead of this pass, which owns that
// transition; a terminal conversation simply stays terminal. Keyed on the
// task's conversations (ListForTask spans the blueprint's step conversations and
// any standalone conversation) rather than on a conversation status.
//
// All-or-nothing per call: any DB error inside the closure rolls back the whole
// batch (flips + audit rows), leaving the artifacts unresolved for a retry on
// the next /undo, /requeue, dismiss, or complete. The flips re-target the same
// predicate set, so retry is safe. All failures are logged, not fatal: the
// calling handler has already flipped the task to its new state.
func (s *Server) teardownTaskArtifacts(ctx context.Context, orgID, userID, taskID string) {
	// Draft PRs captured inside the tx (state already flipped) and closed on GitHub
	// AFTER it commits — a network call must not hold the tx open. Dismissed reviews
	// need no post-tx pass: a review is staged TF-side (TFAC-494), so the in-tx flip
	// to dismissed is the whole resolution — there is no GitHub object to retire.
	//
	// Which credential each draft PR's close is made under is classified BEFORE
	// the tx opens, for the same reason the closes themselves run after it: the
	// audit rows are composed inside that tx, and a classification can reach
	// GitHub.
	credentials := s.draftPRCredentials(ctx, orgID, userID, taskID)

	var prArtifacts []domain.Artifact
	err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		convs, err := tx.Conversations.ListForTask(ctx, orgID, taskID)
		if err != nil {
			return fmt.Errorf("list conversations for task: %w", err)
		}
		for i := range convs {
			conversationID := convs[i].ID
			arts, artErr := tx.Artifacts.ListByConversation(ctx, orgID, conversationID)
			if artErr != nil {
				return fmt.Errorf("artifacts.ListByConversation(%s): %w", conversationID, artErr)
			}
			draftPRs := domain.AllDraftPullRequests(arts)
			pendingReviews := domain.AllPendingReviewArtifacts(arts)
			if len(draftPRs) == 0 && len(pendingReviews) == 0 {
				continue
			}

			// Abandon each pending review by flipping its artifact to dismissed. No
			// GitHub call and no audit row: the review is staged TF-side (TFAC-494), so
			// a dismiss is a purely local state change, not an org-credential write
			// (external_actions records only writes). The flip is the whole teardown;
			// the proposed snapshot is preserved on the row.
			for j := range pendingReviews {
				rv := pendingReviews[j]
				dismissed := rv
				dismissed.State = domain.ArtifactStateReviewDismissed
				if _, err := tx.Artifacts.Upsert(ctx, orgID, dismissed); err != nil {
					return fmt.Errorf("artifacts.Upsert(dismissed): %w", err)
				}
			}
			// Abandon each draft PR by flipping its artifact to closed (the GitHub
			// close runs after the tx). The pushed branch and the proposed snapshot
			// are preserved — abandonment retires the PR object, not the work.
			for j := range draftPRs {
				pr := draftPRs[j]
				closed := pr
				closed.State = domain.ArtifactStatePRClosed
				if _, err := tx.Artifacts.Upsert(ctx, orgID, closed); err != nil {
					return fmt.Errorf("artifacts.Upsert(closed): %w", err)
				}
				if err := tx.ExternalActions.Record(ctx, orgID,
					githubApprovalAction(&pr, userID, domain.ActionPRClosed, domain.ArtifactStatePRDraft, domain.ArtifactStatePRClosed,
						credentialForTarget(credentials, pr.Target))); err != nil {
					return fmt.Errorf("external_actions.Record(closed): %w", err)
				}
				prArtifacts = append(prArtifacts, pr)
			}
		}
		return nil
	})
	if err != nil {
		approvalDiscardLog.Error("task artifact teardown failed; artifacts left unresolved for retry", "task", taskID, "error", err)
		return
	}

	// Resolve on GitHub (best-effort, outside the tx). The artifacts are already
	// marked closed/dismissed; a GitHub failure leaves the object for reconciliation
	// to retire later and must never fail the requeue/complete. Branches stay.
	for i := range prArtifacts {
		closeDraftPRBestEffort(ctx, s.ghResolver, orgID, &prArtifacts[i])
	}
	// Dismissed reviews need no post-tx pass — they were staged TF-side and the
	// in-tx flip is their whole resolution (TFAC-494).
}

// draftPRCredentials classifies the acting GitHub credential for every repo the
// task's unresolved draft PRs live in, keyed by "owner/repo". It exists so the
// teardown's audit rows can name that credential without probing GitHub from
// inside the write tx those rows are composed into — the read pass and the
// classification are both finished before that tx opens.
//
// Best-effort: a read failure yields an empty map and every row falls back to
// the App, exactly as an unclassifiable repo does. The teardown itself is
// unaffected — it re-reads the artifacts under its own tx and is the authority
// on which ones it resolves.
func (s *Server) draftPRCredentials(ctx context.Context, orgID, userID, taskID string) map[string]string {
	type ownerRepo struct{ owner, repo string }
	repos := map[string]ownerRepo{}
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		convs, err := tx.Conversations.ListForTask(ctx, orgID, taskID)
		if err != nil {
			return err
		}
		for i := range convs {
			arts, artErr := tx.Artifacts.ListByConversation(ctx, orgID, convs[i].ID)
			if artErr != nil {
				return artErr
			}
			for _, pr := range domain.AllDraftPullRequests(arts) {
				if owner, repo, _, ok := domain.ParsePRTarget(pr.Target); ok {
					repos[owner+"/"+repo] = ownerRepo{owner: owner, repo: repo}
				}
			}
		}
		return nil
	}); err != nil {
		approvalDiscardLog.Warn("pre-read draft PRs for credential attribution failed; teardown rows will record the app",
			"task", taskID, "error", err)
		return nil
	}
	out := make(map[string]string, len(repos))
	for repoID, or := range repos {
		out[repoID] = githubCredentialFor(ctx, s.ghResolver, orgID, or.owner, or.repo)
	}
	return out
}

// credentialForTarget looks a PR target's repo up in the map draftPRCredentials
// built. A miss — an unparseable target, or a draft PR that appeared between the
// pre-pass and the tx — reports the App, the same answer an unclassifiable repo
// gets.
func credentialForTarget(credentials map[string]string, target string) string {
	owner, repo, _, ok := domain.ParsePRTarget(target)
	if !ok {
		return domain.CredentialGitHubApp
	}
	if cred, hit := credentials[owner+"/"+repo]; hit {
		return cred
	}
	return domain.CredentialGitHubApp
}

// closeDraftPRBestEffort closes the abandoned draft PR on GitHub. Best-effort:
// every failure is logged, never returned — the artifact is already marked
// closed and the conversation/task already resolved, so a GitHub hiccup mustn't
// unwind that. owner/repo/number come from the artifact's target; the per-repo
// client resolves App-installation-token → PAT like every other PR mutation. A
// free function (taking the resolver) so both the task-level teardown and the
// per-artifact dismiss endpoint share one GitHub-resolution path. The pushed
// branch is never touched (retention is a separate concern).
func closeDraftPRBestEffort(ctx context.Context, resolver ghclient.Resolver, orgID string, art *domain.Artifact) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		approvalDiscardLog.Warn("draft PR artifact has a malformed target; skipping GitHub close", "artifact", art.ID, "target", art.Target)
		return
	}
	gh, err := resolver.ClientForRepo(ctx, orgID, owner, repo)
	if err != nil {
		approvalDiscardLog.Warn("resolve github client for draft PR close failed", "artifact", art.ID, "owner", owner, "repo", repo, "error", err)
		return
	}
	if err := gh.ClosePR(ctx, owner, repo, number); err != nil {
		approvalDiscardLog.Warn("close draft PR on github failed (artifact already marked closed)", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	}
}

// revertJiraStateIfApplicable was the body of handleUndo's Jira
// reversal block. Factored so /requeue picks up the same behavior —
// dragging a claimed Jira-backed task back to Queue should unassign
// and transition the ticket the same way Cmd-Z does. The guards
// against external mutations (someone else claimed it, status has
// progressed out of the in-progress rule) apply equally to both
// entry points.
func (s *Server) revertJiraStateIfApplicable(ctx context.Context, orgID, userID string, task *domain.Task) {
	if task == nil || task.EntitySource != "jira" || task.SourceStatus == "" {
		return
	}
	// The requeue/undo reversal (unassign + transition back) reverses
	// the user's own claim, so it must act as that user, not the org service
	// account. Resolve their Jira client. Best-effort: the task is already
	// requeued by the time we get here, so a missing/again-unresolvable user
	// credential is logged and skipped rather than failing the response or
	// degrading to the bot.
	jiraUserClient, jerr := s.jiraResolver.ForUser(ctx, orgID, userID)
	if jerr != nil {
		if errors.Is(jerr, jira.ErrNoJiraUserCredential) {
			jiraLog.Warn("requeue revert: no jira credential for user, skipping ticket", "user", userID, "ticket", task.EntitySourceID)
		} else {
			jiraLog.Warn("requeue revert: resolve user client failed, skipping", "ticket", task.EntitySourceID, "error", jerr)
		}
		return
	}
	// Same hot-path note as the claim route: requeue/undo is human-paced
	// and rule lookup is O(projects). The rule read goes through the
	// app-pool ListForTeam inside a WithTx so jira_rules_select RLS
	// is enforced — matching the user's requeue claim path. If a
	// future profile shows real cost, cache the per-team rules on
	// Server and refresh from onJiraChanged.
	var rule *domain.JiraProjectStatusRules
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		rule = lookupJiraRuleForTask(ctx, tx, task)
		return nil
	}); err != nil {
		jiraLog.Warn("requeue rule lookup failed, skipping revert", "error", err)
		return
	}
	var inProgressMembers []domain.JiraStatusRef
	if rule != nil {
		inProgressMembers = rule.InProgressMembers
	}
	go func(issueKey, originalStatus string, ipMembers []domain.JiraStatusRef) {
		// Detached from the request (see syncJiraClaim's guard): the
		// revert outlives the undo response, so use a background context.
		bgCtx := context.Background()
		state := jiraUserClient.GetClaimState(bgCtx, issueKey)

		// Three assignee cases:
		//   - assigned to someone else -> skip undo entirely (manual reassignment)
		//   - unassigned -> skip Unassign (already unassigned), still transition
		//   - assigned to self -> proceed normally (unassign + transition)
		if state != nil && !state.AssignedToSelf && !state.Unassigned {
			jiraLog.Warn("requeue guard: reassigned to someone else, skipping", "issue", issueKey)
			return
		}
		// Skip if the ticket has moved out of the in-progress rule
		// entirely — that means someone progressed it (to done, back to
		// pickup, etc.) and we shouldn't yank it back. Membership rather
		// than strict-canonical match, because a user moving Claim →
		// "In Review" is still "working on it on my plate" and the
		// requeue should still unwind to the original status.
		if state != nil && len(ipMembers) > 0 && !domain.ContainsStatus(ipMembers, claimStatusRef(state)) {
			jiraLog.Warn("requeue guard: status not in in-progress members, skipping",
				"issue", issueKey, "status", state.StatusName,
				"in_progress_members", domain.JiraStatusNames(ipMembers))
			return
		}

		if state == nil || state.AssignedToSelf {
			if err := jiraUserClient.Unassign(bgCtx, issueKey); err != nil {
				jiraLog.Error("failed to unassign on requeue", "issue", issueKey, "error", err)
			}
		}
		// The target is the status the ticket was in before the claim, recorded
		// on the task as a name — so the transition resolves by name here.
		if err := jiraUserClient.TransitionTo(bgCtx, issueKey, jira.Status{Name: originalStatus}); err != nil {
			jiraLog.Error("failed to transition back on requeue", "issue", issueKey, "status", originalStatus, "error", err)
		}
	}(task.EntitySourceID, task.SourceStatus, inProgressMembers)
}

// parseSnoozeUntilPatch reads the `snooze_until` PATCH field: an RFC3339
// timestamp strictly in the future, or the literal null that clears a snooze.
// Faults land on whichever Validation carries their status: an unparseable
// value is a 400 on `shape`, a past one a 422 on `semantic`.
//
// Timestamps only. The `1h|2h|4h|tomorrow` grammar the snooze picker offers
// lives in the picker now, which resolves a preset to an instant before it
// calls — the presets are wall-clock choices made in the user's own timezone,
// and a server that reads them resolves "tomorrow" against its own clock
// instead. A past wake time is 422 rather than 400: the body is well-formed
// and the value is simply outside the range a wake time can occupy, and the
// row it would produce is one the wake sweep requeues on its next pass — a
// snooze that reports success and does nothing.
func parseSnoozeUntilPatch(shape, semantic *httpx.Validation, raw json.RawMessage) (time.Time, httpx.PatchState) {
	value, state := httpx.PatchString(shape, raw, "snooze_until")
	if state != httpx.PatchSet {
		return time.Time{}, state
	}
	until, err := time.Parse(time.RFC3339, value)
	if err != nil {
		shape.Invalid("snooze_until", "snooze_until must be an RFC3339 timestamp or null")
		return time.Time{}, state
	}
	if !until.After(time.Now()) {
		semantic.OutOfRange("snooze_until", "snooze_until must be in the future")
		return time.Time{}, state
	}
	// UTC on the way out: the wake time is compared against the database's own
	// clock, which is UTC. A local-zone value there reads as its wall clock —
	// the snooze expires the moment it is written (west of UTC) or outlasts
	// its duration by the offset (east of it).
	return until.UTC(), state
}

// validateHesitation rejects a negative hesitation. It is the milliseconds a
// user spent deciding before the gesture, so below zero is not a slow decision
// but a broken clock or a hand-written body — and it lands in swipe_events,
// where it skews the dwell-time aggregates nothing downstream re-validates.
// Shared by every route that accepts the field, so the three gestures reject
// the same values with the same fault.
func validateHesitation(v *httpx.Validation, ms int) {
	if ms < 0 {
		v.OutOfRange("hesitation_ms", "hesitation_ms must be zero or greater")
	}
}
