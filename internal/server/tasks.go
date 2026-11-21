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

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// taskJSON is the API representation of a task. Maps entity-joined fields
// to the frontend's expected shape for backward compatibility.
type taskJSON struct {
	ID                  string   `json:"id" doc:"The task's id."`
	EntityID            string   `json:"entity_id" doc:"The id of the entity the task is about: a pull request, an issue, a vulnerable package, or a Slack thread or message."` // FK to entities.id — lets callers correlate tasks back to their entity
	Source              string   `json:"source" doc:"The system the task's entity comes from, such as github or jira."`                                                         // from entity
	SourceID            string   `json:"source_id" doc:"The entity's key in its source, such as owner/repo#42 for a pull request or PROJ-123 for a Jira issue."`                // from entity
	SourceURL           string   `json:"source_url" doc:"The entity's page in its source."`                                                                                     // from entity
	Title               string   `json:"title" doc:"The entity's title."`                                                                                                       // from entity
	EntityKind          string   `json:"entity_kind" doc:"The kind of entity the task is about: pr, issue, vulnerable_package, thread, or message."`                            // "pr" | "issue"
	EventType           string   `json:"event_type" doc:"The event type that created the task, such as github:pr:review_requested."`
	DedupKey            string   `json:"dedup_key,omitempty" doc:"The value that tells apart tasks of one event type on one entity, such as a label name, absent for most event types."`
	Severity            string   `json:"severity,omitempty" doc:"The task's severity, absent when none is recorded."`
	RelevanceReason     string   `json:"relevance_reason,omitempty" doc:"Why the task concerns the person it was routed to, one of review_requested, authored, mentioned, and assigned, absent when none applies."`
	ScoringStatus       string   `json:"scoring_status" doc:"Where the task is in priority scoring: pending, in_progress, or scored."`
	CreatedAt           string   `json:"created_at" doc:"When the task was created, as an RFC 3339 timestamp."`
	Status              string   `json:"status" doc:"The task's lane: queued, in_progress, done, dismissed, or snoozed."`
	PriorityScore       *float64 `json:"priority_score" doc:"How urgently the task needs a person's attention, from 0 to 1, or null before the task is scored."`
	AutonomySuitability *float64 `json:"autonomy_suitability" doc:"How suitable the task is for an agent to work without a person, from 0 to 1, or null before the task is scored."`
	AISummary           string   `json:"ai_summary,omitempty" doc:"A summary of the task written when it was scored, absent before then."`
	PriorityReasoning   string   `json:"priority_reasoning,omitempty" doc:"The reasoning behind priority_score, absent before the task is scored."`
	CloseReason         string   `json:"close_reason,omitempty" doc:"Why the task closed, such as run_completed, user_completed, user_dismissed, or auto_closed_by_event, absent while it is open."`
	// SnoozeUntil — populated when the task is in a snoozed state.
	// A snoozed task holds no claim (the tasks_queue_unclaimed CHECK),
	// so this is only ever set on queue-lane tasks. Any claim-axis
	// transition wakes the task atomically (clears snooze_until +
	// lands the row in_progress), so claimed cards on the
	// Board never carry a snooze. The Cards triage view renders
	// future-snoozed entries hidden via the TaskListFilter status filter;
	// the Board's Queue lane could optionally render them at the
	// tail with a "wakes Mar 5" badge (deferred UI follow-up).
	SnoozeUntil string `json:"snooze_until,omitempty" doc:"When a snoozed task returns to the queue, as an RFC 3339 timestamp, absent when the task is not snoozed."`
	// OpenSubtaskCount lets the UI flag a task whose Jira entity has open
	// subtasks — the "consider decomposing" signal. Zero for
	// GitHub tasks and Jira tickets without subtasks.
	OpenSubtaskCount int `json:"open_subtask_count" doc:"How many open subtasks the task's Jira issue has, 0 for any other entity."`
	// Claim cols: exposed so the per-card assignee picker
	// can render the current assignee without a second round-trip.
	// Exactly one is set when claimed; both empty when unclaimed.
	// omitempty keeps the wire shape clean for the unclaimed-queue case.
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty" doc:"The id of the agent that holds the task, absent when no agent does."`
	ClaimedByUserID  string `json:"claimed_by_user_id,omitempty" doc:"The id of the person who holds the task, absent when no person does."`
	// TeamID is the task's owning team. Exposed so the multi-team board
	// can color-code / tag rows by team. Always set; the
	// frontend only surfaces it when the viewer belongs to ≥2 teams.
	// TODO: board row color-coding consumes this.
	TeamID string `json:"team_id,omitempty" doc:"The id of the team that owns the task, absent while no single team owns it."`
	// Version is the task's optimistic-concurrency token (domain.Task.Version).
	// Every task write requires the value the caller read and is refused with
	// 409 VERSION_CONFLICT once the task has moved past it; every write's
	// response carries the new one, so a client can chain writes without a
	// read between them. Not omitempty: it is what the next write sends.
	Version int64 `json:"version" doc:"The task's version, which a write to the task must send and which each write raises."`
	// RequestedReviewer is, on a github:pr:review_requested task, the
	// requested user's login or team's org/slug as the event that spawned the
	// task named it, for display. dedup_key identifies the reviewer, by
	// GitHub's id ("user_id:<id>" / "team_id:<id>") when the request carried
	// one, and pins it, so every event that bumps the task names the same one.
	// Omitted on every other task.
	RequestedReviewer string `json:"requested_reviewer,omitempty" doc:"On a github:pr:review_requested task, the requested user's login or the requested team's org/slug, absent on every other task."`
	// VulnerablePackage is, on a task whose entity_kind is
	// "vulnerable_package", the package and the Dependabot alerts open on it.
	// Omitted on every other task.
	VulnerablePackage *vulnerablePackageJSON `json:"vulnerable_package,omitempty" doc:"The package and its open Dependabot alerts on a task whose entity_kind is vulnerable_package, absent on every other task."`
	// ConversationID is the task's current conversation: the one a person's
	// claim minted on a task a person holds, the agent's on a task the agent
	// holds. Absent on a task nobody holds, and on one whose queued firing has
	// not started its run.
	ConversationID string `json:"conversation_id,omitempty" doc:"The id of the task's current conversation, absent when nobody holds the task or its run has not started."`
	// MemoryPending says a conversation on this task ended without leaving
	// the memory its successor is owed, and the dispatcher claims none of the
	// task's conversations until one exists. Not omitempty: false is a real
	// answer about the task, and a flag that vanishes when false reads as one
	// the server failed to compute.
	MemoryPending bool `json:"memory_pending" doc:"Whether a conversation on the task ended without the memory the next conversation needs, in which case no further conversation on the task starts until that memory exists."`
	// MemoryAttempt is the newest try at that memory — absent when none has
	// run yet, which is the ordinary case for a wait that has just begun.
	MemoryAttempt *memoryAttemptJSON `json:"memory_attempt,omitempty" doc:"The newest attempt to write that memory, absent when none has run."`
	// CanWrite is whether the caller may write the task (PATCH, claim,
	// delegate, requeue), from the check those routes run
	// (authz.Checker.UserCanWriteTask, and WritableTasks for a page of them),
	// so a client offers a task write only to a caller the route would
	// accept. On the detail read and on each row of the list. Absent from the
	// task a write answers with, and when the check fails, which a client
	// reads as unknown.
	CanWrite *bool `json:"can_write,omitempty" doc:"Whether the caller may update, claim, delegate, or requeue the task, present on the task reads and absent from a write's response or when the check fails."`
	// ClaimTeamIDs is the teams the caller's claim of the task, or their
	// delegate of it, may land it on (db.TaskStore.ClaimTeamCandidates), in
	// the order that read gives: what those routes accept as team_id, and
	// with one entry the team they resolve an absent team_id to. On the
	// detail read only. A pointer so that no candidate is an empty list and
	// a failed read is an absent key, which a client reads as unknown.
	ClaimTeamIDs *[]string `json:"claim_team_ids,omitempty" doc:"The ids of the teams the caller's claim or delegation may put the task on, in order, present on the single-task read only and absent when that read fails."`
}

// memoryAttemptJSON is the wire shape of the newest memory-generation attempt
// on a pending task: what came of it, why it stopped, and when it started. Its
// own type rather than domain.MemoryAttemptSummary on the wire, so the
// timestamp is rendered the way every other timestamp on this response is (a
// formatted string, not a marshalled time.Time) and the domain struct stays
// free to grow a field the API has not decided to publish.
type memoryAttemptJSON struct {
	// Outcome is empty while the attempt is still running.
	Outcome string `json:"outcome,omitempty" doc:"What the attempt came to: generated, empty, or failed, absent while it is running."`
	// ErrorKind and ErrorMessage are set exactly when the outcome is `failed`.
	// The message is TF's own wording, never an upstream response body.
	ErrorKind    string `json:"error_kind,omitempty" doc:"Why the attempt failed, present only when outcome is failed."`
	ErrorMessage string `json:"error_message,omitempty" doc:"A description of the failure, present only when outcome is failed."`
	StartedAt    string `json:"started_at" doc:"When the attempt started, as an RFC 3339 timestamp."`
}

// vulnerablePackageJSON is what a vulnerable-package task shows of its
// package: the repository, ecosystem and name, and the alerts open on it when
// TF last listed the repository. The three names are read from the entity's
// key, which a repository rename rewrites, rather than from its snapshot,
// which the next listing does.
type vulnerablePackageJSON struct {
	Repo      string `json:"repo" doc:"The repository, as owner/name."` // owner/name
	Ecosystem string `json:"ecosystem" doc:"The package's ecosystem, such as npm or pip."`
	Package   string `json:"package" doc:"The package's name."`
	// HighestSeverity is the highest severity among the open alerts, one of
	// low, medium, high and critical. Absent when no open alert carries one of
	// those four, which includes a package whose last alert closed.
	HighestSeverity string `json:"highest_severity,omitempty" doc:"The highest severity among the open alerts, one of low, medium, high, and critical, absent when no open alert has one of those."`
	OpenAlertCount  int    `json:"open_alert_count" doc:"How many alerts are open on the package."`
	// Alerts are the open alerts, highest severity first and by number within
	// a severity. Never null.
	Alerts []dependabotAlertJSON `json:"alerts" doc:"The open alerts, highest severity first and then by number."`
}

// dependabotAlertJSON is one open Dependabot alert. summary is the advisory's,
// written outside the org, so a client renders it as plain text.
type dependabotAlertJSON struct {
	Number int    `json:"number" doc:"The alert's number in its repository."`
	URL    string `json:"url" doc:"The alert's page on GitHub."` // the alert's page on GitHub
	GHSAID string `json:"ghsa_id" doc:"The advisory's GitHub Security Advisory id."`
	CVEID  string `json:"cve_id,omitempty" doc:"The advisory's CVE id, absent when it has none."`
	// Summary is absent when the advisory has none.
	Summary      string `json:"summary,omitempty" doc:"The advisory's summary, to be shown as plain text, absent when the advisory has none."`
	Severity     string `json:"severity" doc:"The advisory's severity: low, medium, high, or critical."`
	ManifestPath string `json:"manifest_path" doc:"The path of the manifest file that declares the package."`
	// Scope is runtime or development, absent when GitHub does not know.
	Scope                  string `json:"scope,omitempty" doc:"Whether the dependency is runtime or development, absent when GitHub does not know."`
	VulnerableVersionRange string `json:"vulnerable_version_range,omitempty" doc:"The range of versions the advisory affects, absent when GitHub reports none."`
	// FirstPatchedVersion is absent when no fixed version has been released.
	FirstPatchedVersion string `json:"first_patched_version,omitempty" doc:"The first version that fixes the advisory, absent when none has been released."`
}

func vulnerablePackageToJSON(sourceID string, p *domain.TaskVulnerablePackage) *vulnerablePackageJSON {
	if p == nil {
		return nil
	}
	snap := p.Snapshot
	repo, ecosystem, name, ok := domain.SplitVulnerablePackageSourceID(sourceID)
	if !ok {
		repo, ecosystem, name = snap.Repo, snap.Ecosystem, snap.Package
	}
	alerts := slices.Clone(snap.Alerts)
	slices.SortStableFunc(alerts, func(a, b domain.DependabotAlert) int {
		if d := domain.DependabotSeverityRank(b.Severity) - domain.DependabotSeverityRank(a.Severity); d != 0 {
			return d
		}
		return a.Number - b.Number
	})
	out := &vulnerablePackageJSON{
		Repo:            repo,
		Ecosystem:       ecosystem,
		Package:         name,
		HighestSeverity: snap.HighestSeverity(),
		OpenAlertCount:  len(alerts),
		Alerts:          make([]dependabotAlertJSON, len(alerts)),
	}
	for i, a := range alerts {
		out.Alerts[i] = dependabotAlertJSON{
			Number:                 a.Number,
			URL:                    domain.DependabotAlertURL(p.Host, repo, a.Number),
			GHSAID:                 a.GHSAID,
			CVEID:                  a.CVEID,
			Summary:                a.Summary,
			Severity:               a.Severity,
			ManifestPath:           a.ManifestPath,
			Scope:                  a.Scope,
			VulnerableVersionRange: a.VulnerableVersionRange,
			FirstPatchedVersion:    a.FirstPatchedVersion,
		}
	}
	return out
}

func memoryAttemptToJSON(a *domain.MemoryAttemptSummary) *memoryAttemptJSON {
	if a == nil {
		return nil
	}
	return &memoryAttemptJSON{
		Outcome:      string(a.Outcome),
		ErrorKind:    string(a.ErrorKind),
		ErrorMessage: a.ErrorMessage,
		StartedAt:    a.StartedAt.Format(time.RFC3339),
	}
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
		RequestedReviewer:   t.RequestedReviewer,
		VulnerablePackage:   vulnerablePackageToJSON(t.EntitySourceID, t.VulnerablePackage),
		ClaimedByAgentID:    t.ClaimedByAgentID,
		ClaimedByUserID:     t.ClaimedByUserID,
		TeamID:              teamIDString(t.TeamID),
		Version:             t.Version,
		ConversationID:      t.ConversationID,
		MemoryPending:       t.MemoryPending,
		MemoryAttempt:       memoryAttemptToJSON(t.MemoryAttempt),
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
	Statuses []string `json:"statuses" doc:"Lanes to include, from queued, in_progress, done, dismissed, and snoozed; empty includes every lane."`
	// TeamIDs is the per-page multi-team view scope. Empty = the union of
	// the viewer's teams (the RLS-scoped default). Well-formedness is
	// checked strictly: a corrupt filter must not silently widen back to
	// the union.
	TeamIDs []string `json:"team_ids" doc:"Ids of the teams whose tasks to include; empty includes every team the caller belongs to."`
	// OnlyUnclaimed keeps just the rows nobody has taken.
	OnlyUnclaimed bool `json:"only_unclaimed" doc:"Whether to include only tasks nobody holds."`
	// IncludeSnoozed keeps rows still inside their snooze window.
	IncludeSnoozed bool `json:"include_snoozed" doc:"Whether to include tasks still inside their snooze window."`
	// ClosedSince (RFC3339) windows done/dismissed rows by closed_at.
	// Absent = no window. The board asks for the seven days it wants to
	// render; the server no longer applies one behind the caller's back.
	ClosedSince string `json:"closed_since" doc:"An RFC 3339 timestamp that limits done and dismissed tasks to those closed at or after it."`
	// CreatedSince (RFC3339) keeps rows created at or after it, whatever
	// their status — combined with a count-only page it is the "tasks over
	// the last N days" figure. Absent = no window.
	CreatedSince string `json:"created_since" doc:"An RFC 3339 timestamp that limits the result to tasks created at or after it."`
	// Sources narrows to tasks whose entity came from these sources.
	// Validated against the event catalog's source vocabulary
	// (domain.EventSources); an unknown value is a client fault, not an
	// empty page. Empty = all sources.
	Sources []string `json:"sources" doc:"Sources whose tasks to include, such as github or jira; empty includes every source."`
	// EntityKinds narrows to tasks whose entity is of these kinds, as a
	// lane of vulnerable packages is. Validated against domain.EntityKinds
	// on the same terms as Sources. Empty = every kind.
	EntityKinds []string `json:"entity_kinds" doc:"Entity kinds whose tasks to include, such as pr or vulnerable_package; empty includes every kind."`
}

// taskListRequest is the body of POST /api/tasks/list: the lane, the reader's
// own narrowing of it, and the shared paging fields.
type taskListRequest struct {
	taskLaneRequest

	// IDs narrows to the tasks with these ids, for a caller that already
	// holds the ids of the tasks it shows. Validated by taskListIDs. Empty =
	// every id.
	IDs []string `json:"ids" doc:"Ids of the tasks to include, at most 200, each a UUID in lowercase hyphenated form; empty includes every task."`
	// EntityIDs narrows to the tasks on these entities, for a caller that
	// holds the entities it shows and wants their work. Validated like IDs.
	// Empty = every entity.
	EntityIDs []string `json:"entity_ids" doc:"Ids of the entities whose tasks to include, at most 200, each a UUID in lowercase hyphenated form; empty includes every entity."`
	// CreatedBefore (RFC3339) is CreatedSince's other half: rows created at
	// or before it. The pair is how a lane asks for a window rather than a
	// ray, and either end may stand alone. It sits with the reader's own
	// narrowing rather than in the lane: it is the moving end of the window
	// a column's filter popover offers, so a facet computed under it would
	// describe the reader's slice instead of the column.
	CreatedBefore string `json:"created_before" doc:"An RFC 3339 timestamp that limits the result to tasks created at or before it."`
	// EventTypes narrows to these stations. Validated against the catalog
	// (domain.EventTypeIDs) on the same terms as Sources. Empty = all.
	EventTypes []string `json:"event_types" doc:"Event types whose tasks to include, from the catalog GET /api/event-types returns; empty includes every event type."`
	// Search is a case-insensitive substring the row must carry in its
	// title, source id, ai_summary or event type. Trimmed; empty after the
	// trim is absent. It runs here rather than in the client because a lane
	// holds one page: a match on an unfetched page is invisible to a
	// client-side filter, and the tail's total would answer a different
	// query from the items above it.
	Search string `json:"search" doc:"Text the task's title, source id, summary, or event type must contain, matched without regard to case."`
	// SortKey reorders the lane (db.TaskListSortKeys). Absent asks for the
	// store's default order — there is deliberately no "default" value to
	// send, since a name for the absent case is a second spelling of it.
	SortKey string `json:"sort_key" doc:"Orders the result by title, created, event_type, or claimee; absent orders it by the default, open tasks needing attention first and then by priority."`
	// SortDir is SortKey's direction. Absent alongside a key means desc.
	// Alone it is refused rather than ignored: the default order has no
	// direction, so a lone sort_dir is a caller expecting something the
	// answer will not show.
	SortDir string `json:"sort_dir" doc:"The direction of sort_key, asc or desc, desc when absent; it is refused without sort_key."`

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
		Statuses:       httpx.CanonicalStrings(req.Statuses),
		TeamIDs:        httpx.CanonicalStrings(req.TeamIDs),
		OnlyUnclaimed:  req.OnlyUnclaimed,
		IncludeSnoozed: req.IncludeSnoozed,
		Sources:        httpx.CanonicalStrings(req.Sources),
		EntityKinds:    httpx.CanonicalStrings(req.EntityKinds),
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
	validKinds := domain.EntityKinds()
	for _, kind := range f.EntityKinds {
		if !slices.Contains(validKinds, kind) {
			v.Invalid("entity_kinds", fmt.Sprintf("unknown entity kind %q; must be one of: %s",
				kind, strings.Join(validKinds, ", ")))
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

// maxTaskListIDs caps how many ids one task-list read names. It is one full
// page, so a read that names its tasks and asks for that many rows gets every
// one of them on its first page.
const maxTaskListIDs = httpx.MaxPageSize

// taskListIDs validates the list's ids filter and returns it canonicalized.
// Each id must be a task id in canonical form (httpx.IsCanonicalID): SQLite
// compares the text, so another spelling of the same uuid would match nothing
// and answer an empty page for a filter the caller believes is right. The cap
// counts distinct ids, so a repeated id does not spend it.
func taskListIDs(v *httpx.Validation, raw []string) []string {
	for i, id := range raw {
		if !httpx.IsCanonicalID(id) {
			v.Invalid(fmt.Sprintf("ids[%d]", i), fmt.Sprintf("%q is not a task id in canonical form", id))
		}
	}
	ids := httpx.CanonicalStrings(raw)
	if len(ids) > maxTaskListIDs {
		v.OutOfRange("ids", fmt.Sprintf("ids accepts at most %d task ids per request; got %d", maxTaskListIDs, len(ids)))
	}
	return ids
}

// taskListFilterKey is the canonicalized form of a taskListRequest's filters —
// sorted, deduped, and normalized — that the page token is fingerprinted
// against. Two requests that mean the same query fingerprint identically, and
// a token minted for one filter set is refused for another.
type taskListFilterKey struct {
	Statuses       []string `json:"statuses"`
	IDs            []string `json:"ids"`
	EntityIDs      []string `json:"entity_ids"`
	TeamIDs        []string `json:"team_ids"`
	OnlyUnclaimed  bool     `json:"only_unclaimed"`
	IncludeSnoozed bool     `json:"include_snoozed"`
	ClosedSince    string   `json:"closed_since"`
	CreatedSince   string   `json:"created_since"`
	CreatedBefore  string   `json:"created_before"`
	Sources        []string `json:"sources"`
	EntityKinds    []string `json:"entity_kinds"`
	EventTypes     []string `json:"event_types"`
	Search         string   `json:"search"`
	// The sort is part of the key for the same reason the filters are: an
	// offset addresses a position in an ordering, so a token minted under
	// one sort names different rows under another.
	SortKey string `json:"sort_key"`
	SortDir string `json:"sort_dir"`
}

// taskListSortKeys is db.TaskListSortKeys as the list route's sort vocabulary.
var taskListSortKeys = func() []httpx.SortKey {
	keys := make([]httpx.SortKey, len(db.TaskListSortKeys))
	for i, k := range db.TaskListSortKeys {
		keys[i] = httpx.SortKey{Name: k, Default: db.SortDesc}
	}
	return keys
}()

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
	EntityID  string `json:"entity_id" doc:"The id of the entity the task is about, which must be active. It is required."`
	EventType string `json:"event_type" doc:"The event type the task is for, which an event recorded on the entity must have. It is required."`
	DedupKey  string `json:"dedup_key" doc:"The value that tells apart tasks of one event type on one entity, such as a label name, which the event must also have; empty for most event types."`
	TeamID    string `json:"team_id" doc:"The id of the team to create the task for; absent uses the team the caller last wrote to, or their only team."`
}

var opTaskCreate = httpx.NewJSONOp[taskCreateRequest, taskJSON](httpx.OpSpec{
	Pattern:       "POST /api/tasks",
	ID:            "create_task",
	Summary:       "Create a task",
	Description:   "Returns the open task for the entity, event type, and dedup key on the acting team, creating it when there is none, anchored on the newest matching event recorded on the entity. Answers 201 when it creates the task and 200 when it finds one. It starts no run; delegating the task is POST /api/tasks/{id}/delegate.",
	Status:        http.StatusCreated,
	OtherStatuses: []int{http.StatusOK},
	Hints:         httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
//   - the entity must exist (404) and be active (409) — the factory keeps a
//     closed entity on the floor for a minute so its chip can ride the final
//     animation hop after it flipped to merged/closed, and a task synthesized
//     there would run to completion against a closed PR with no close-cascade
//     to clean it up;
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
	req, ok := opTaskCreate.Decode(w, r)
	if !ok {
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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	//
	// An active task can stand at a station no event names: a Jira status or
	// priority change task keyed on the status's or priority's id, opened by
	// events that carry its name as their dedup_key. That task already has its
	// anchor, so it is the answer, as the find branch below would give it.
	var (
		primaryEvent *domain.Event
		standing     *domain.Task
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		primaryEvent, e = tx.Events.LatestForEntityTypeAndDedupKey(r.Context(), orgID, req.EntityID, req.EventType, req.DedupKey)
		if e != nil || primaryEvent != nil {
			return e
		}
		active, e := tx.Tasks.FindActiveByEntityAndType(r.Context(), orgID, req.EntityID, req.EventType)
		if e != nil {
			return e
		}
		for i := range active {
			if active[i].DedupKey == req.DedupKey {
				standing = &active[i]
				break
			}
		}
		return nil
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if standing != nil {
		opTaskCreate.WriteStatus(w, http.StatusOK, taskToJSON(*standing))
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
		if errors.Is(err, db.ErrEntityClosed) {
			// The station named an entity whose work is over, or none at
			// all — a semantic fault in what was referenced, the same class
			// as the missing event above, and one the caller can see for
			// themselves.
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: "entity is closed or does not exist; no task can be created on it",
				Field:   "entity_id",
			})
			return
		}
		internalError(w, "tasks", err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	opTaskCreate.WriteStatus(w, status, taskToJSON(*task))
}

// teamDefaultPriority is the priority a task gets at creation when no scorer has
// run yet: the highest default among the handlers that make a task, belong to
// teamID and match this event, or 0.5 when none does. It mirrors the
// predicate-match filter in internal/routing so a task minted by hand lands at
// the same priority the router would have given one minted by a poll.
//
// Only handlers belonging to the acting team count: a handler owned by another
// team must not lift the priority of a task created for this one. The predicate
// gate stays for the same reason in the other direction — iterating every
// active handler would inflate priority whenever a high-priority handler's
// scope_predicate doesn't match this event's metadata. Empty predJSON always
// matches, per the events package contract.
func teamDefaultPriority(ctx context.Context, tx db.TxStores, orgID, teamID, eventType string, primaryEvent *domain.Event, schema events.EventSchema, schemaOK bool) (float64, error) {
	priority, matchedAny := 0.5, false
	handlers, err := tx.EventHandlers.GetActiveForEvent(ctx, orgID, eventType)
	if err != nil {
		return 0, err
	}
	metadataJSON, err := events.Current(ctx, orgID, eventType, primaryEvent.MetadataJSON)
	if err != nil {
		return 0, err
	}
	for _, h := range handlers {
		if !h.MakesTask() {
			continue
		}
		if h.TeamID != "" && h.TeamID != teamID {
			continue
		}
		if !schemaOK {
			// No registered schema → the predicate can't be evaluated. Mirrors
			// matchPredicate's quietly-permissive behavior: skip the handler.
			continue
		}
		predJSON := ""
		if h.ScopePredicateJSON != nil {
			predJSON = *h.ScopePredicateJSON
		}
		matched, merr := schema.Match(predJSON, metadataJSON)
		if merr != nil {
			tasksLog.Warn("event_handler predicate error, skipping", "event_handler", h.ID, "error", merr)
			continue
		}
		if matched && (!matchedAny || h.DefaultPriority > priority) {
			priority, matchedAny = h.DefaultPriority, true
		}
	}
	return priority, nil
}

var opTaskList = httpx.NewListOp[taskListRequest, taskJSON](httpx.OpSpec{
	Pattern:     "POST /api/tasks/list",
	ID:          "list_tasks",
	Summary:     "List tasks",
	Description: "Returns one page of the tasks the caller can see, narrowed by the body's filters and ordered by sort_key, or without one by attention and then priority. Each task says whether the caller may write it. A page_token resumes after the last task of the page that returned it, and is refused for different filters.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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

	req, ok := opTaskList.Decode(w, r)
	if !ok {
		return
	}

	var v httpx.Validation
	filter, laneKeys := resolveTaskLane(&v, req.taskLaneRequest)
	filter.IDs = taskListIDs(&v, req.IDs)
	filter.EntityIDs = canonicalIDList(&v, "entity_ids", "an entity", req.EntityIDs)
	var createdBeforeKey string
	filter.CreatedBefore, createdBeforeKey = parseTaskWindow(&v, "created_before", req.CreatedBefore)
	filter.EventTypes = httpx.CanonicalStrings(req.EventTypes)
	validEventTypes := domain.EventTypeIDs()
	for _, et := range filter.EventTypes {
		if !slices.Contains(validEventTypes, et) {
			// The catalog is too long to spell into an error message, and it
			// is already a route: name the offending value and say where the
			// vocabulary lives.
			v.Invalid("event_types", fmt.Sprintf("unknown event type %q; see GET /api/event-types for the catalog", et))
		}
	}
	filter.Search = httpx.Search(&v, req.Search)
	// Newest / Z-A first whatever the key, which is what the board's
	// controls default to.
	order := httpx.Sort(&v, req.SortKey, req.SortDir, taskListSortKeys)
	filter.SortKey, filter.SortDir = order.Key, order.Dir
	page := httpx.ResolveKeysetPage(&v, req.PageRequest, httpx.FilterFingerprint(taskListFilterKey{
		Statuses:       filter.Statuses,
		IDs:            filter.IDs,
		EntityIDs:      filter.EntityIDs,
		TeamIDs:        filter.TeamIDs,
		OnlyUnclaimed:  filter.OnlyUnclaimed,
		IncludeSnoozed: filter.IncludeSnoozed,
		ClosedSince:    laneKeys.ClosedSince,
		CreatedSince:   laneKeys.CreatedSince,
		CreatedBefore:  createdBeforeKey,
		Sources:        filter.Sources,
		EntityKinds:    filter.EntityKinds,
		EventTypes:     filter.EventTypes,
		Search:         httpx.SearchKey(filter.Search),
		SortKey:        filter.SortKey,
		SortDir:        filter.SortDir,
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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	s.addTaskCanWrite(r.Context(), orgID, userID, items)
	opTaskList.WriteKeyset(w, page, items, total, nextKey)
}

// addTaskCanWrite puts the write gate's answer on each task of a list page,
// as `can_write`, so the board offers the assignee picker and the drag only
// on a card the claim, requeue, and delegate routes would accept. One query
// for the page (authz.Checker.WritableTasks), the predicate the detail read's
// probe runs.
//
// Best-effort, like the detail read's: a failed probe omits the key on every
// row, which a client reads as unknown rather than as a refusal. It runs after
// the read's transaction because the probe opens its own.
func (s *Server) addTaskCanWrite(ctx context.Context, orgID, userID string, items []taskJSON) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	writable, err := s.az.WritableTasks(ctx, userID, orgID, ids)
	if err != nil {
		serverLog.Warn("task-write probe failed; omitting can_write", "error", err)
		return
	}
	for i := range items {
		canWrite := writable[items[i].ID]
		items[i].CanWrite = &canWrite
	}
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

	IDs           json.RawMessage `json:"ids" spec:"-"`
	EventTypes    json.RawMessage `json:"event_types" spec:"-"`
	Search        json.RawMessage `json:"search" spec:"-"`
	SortKey       json.RawMessage `json:"sort_key" spec:"-"`
	SortDir       json.RawMessage `json:"sort_dir" spec:"-"`
	CreatedBefore json.RawMessage `json:"created_before" spec:"-"`
	PageSize      json.RawMessage `json:"page_size" spec:"-"`
	PageToken     json.RawMessage `json:"page_token" spec:"-"`
}

// taskFacetsResponse is the fixed schema of named cuts a synthetic read
// answers with. A further cut lands beside these keys without a wire change,
// and never as a `group_by=` parameter that would reshape the response.
//
// Each cut is computed under every lane filter but its own: entity_kinds
// counts the lane with the request's entity_kinds left out, so the kind
// choices show the kinds a reader just narrowed away, while event_types
// counts the lane as narrowed, kind included.
type taskFacetsResponse struct {
	EventTypes  []httpx.FacetJSON `json:"event_types" doc:"Each event type the lane's tasks have, with how many tasks have it, counted under every filter in the body."`
	EntityKinds []httpx.FacetJSON `json:"entity_kinds" doc:"Each entity kind the lane's tasks are about, with how many tasks are about it, counted under every filter in the body except entity_kinds."`
}

var opTaskFacets = httpx.NewJSONOp[taskFacetsRequest, taskFacetsResponse](httpx.OpSpec{
	Pattern:     "POST /api/tasks/facets",
	ID:          "get_task_facets",
	Summary:     "Count a lane's tasks by event type and entity kind",
	Description: "Returns the event types and entity kinds among the tasks the caller can see in a lane, the set the lane filters of POST /api/tasks/list describe, with how many tasks have each. It takes only those lane filters, and refuses the list's other fields and its paging fields by name.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleTaskFacets is the tasks list's synthetic sibling: the event types and
// the entity kinds present in a lane and how many rows carry each — the sets
// a lane's filter chips are drawn from.
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
// No paging and no total_count: each grouping is over a closed vocabulary, so
// the answer is bounded by it rather than by a window. Authorization
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

	req, ok := opTaskFacets.Decode(w, r)
	if !ok {
		return
	}

	var v httpx.Validation
	const narrows = "%s narrows a lane; the facet answers for the whole lane, including the values a reader filtered out"
	const pages = "%s is not accepted: the facets are one bounded answer over closed vocabularies, not a page"
	for _, refused := range []struct {
		field  string
		sent   json.RawMessage
		reason string
	}{
		{"ids", req.IDs, narrows},
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

	kindsLane := filter
	kindsLane.EntityKinds = nil
	var eventTypes, kinds []db.Facet
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if eventTypes, e = tx.Tasks.FacetEventTypes(r.Context(), orgID, filter); e != nil {
			return e
		}
		kinds, e = tx.Tasks.FacetEntityKinds(r.Context(), orgID, kindsLane)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	opTaskFacets.Write(w, taskFacetsResponse{
		EventTypes:  httpx.Facets(eventTypes),
		EntityKinds: httpx.Facets(kinds),
	})
}

var opTaskGet = httpx.NewJSONOp[httpx.NoBody, taskJSON](httpx.OpSpec{
	Pattern:     "GET /api/tasks/{id}",
	ID:          "get_task",
	Summary:     "Get a task",
	Description: "Returns one task the caller can see, with whether the caller may write it and the teams the caller's claim or delegation of it may put it on.",
	PathParams:  map[string]string{"id": "The task's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	out := taskToJSON(*task)
	// Best-effort, like the other display enrichment on a read: a failed
	// probe omits the field rather than failing the read of a row the caller
	// can see. It runs after the read's transaction because the probe opens
	// its own, and so does the candidates read below, whose failure must not
	// take the task's read with it.
	if canWrite, err := s.az.UserCanWriteTask(r.Context(), userID, orgID, task.ID); err != nil {
		serverLog.Warn("task-write probe failed; omitting can_write", "task", task.ID, "error", err)
	} else {
		out.CanWrite = &canWrite
	}
	candidates := []string{}
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teams, e := tx.Tasks.ClaimTeamCandidates(r.Context(), orgID, task.ID, userID)
		candidates = append(candidates, teams...)
		return e
	}); err != nil {
		serverLog.Warn("claim-team read failed; omitting claim_team_ids", "task", task.ID, "error", err)
	} else {
		out.ClaimTeamIDs = &candidates
	}
	opTaskGet.Write(w, out)
}

// Task status vocabulary, split by who owns each write. Terminal statuses
// close a task; in_progress is where a claim lands a task, written by the
// claim doors themselves — assigning work, to a person or to the bot, is what
// starts it.
const (
	taskStatusQueued     = "queued"
	taskStatusInProgress = "in_progress"
	taskStatusSnoozed    = "snoozed"
	taskStatusDone       = "done"
	taskStatusDismissed  = "dismissed"
)

// isTerminalTaskStatus reports whether a task is closed. A closed task takes
// no further mutation except the route that re-opens it (requeue), so every
// other handler refuses on this predicate rather than writing over
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
	Status      httpx.Patch[string] `json:"status" doc:"The lane to move the task to: done or dismissed, which close it, or in_progress, which needs the caller to hold the task. It cannot be null or sent with snooze_until."`
	SnoozeUntil httpx.Patch[string] `json:"snooze_until" doc:"When the task returns to the queue, as an RFC 3339 timestamp in the future, which snoozes an open task nobody holds; null returns a snoozed task to the queue now. It cannot be sent with status."`
	// Version is the task version the caller read (taskJSON.Version). Not a
	// patch field: it cannot be cleared, and it says which task the fields
	// above were decided against.
	Version *int64 `json:"version" doc:"The version the caller's read of the task returned; the write is refused when the task has changed since. It is required."`
}

// taskWriter is a task write's operation, which answers with the task as the
// reads serve it.
type taskWriter interface {
	Write(w http.ResponseWriter, resp taskJSON)
}

var opTaskUpdate = httpx.NewJSONOp[taskPatchRequest, taskJSON](httpx.OpSpec{
	Pattern:     "PATCH /api/tasks/{id}",
	ID:          "update_task",
	Summary:     "Update a task",
	Description: "Closes the task as done or dismissed, snoozes it until a time, returns a snoozed task to the queue, or confirms in_progress on a task the caller holds, and returns the task with the change applied. Closing a task tells any run working on it that it closed; the run is not stopped.",
	PathParams:  map[string]string{"id": "The task's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// taskWriteVersion reads the version a task write names: the one the
// caller's read of the task returned. Every task write route requires it,
// because without it the write lands on whatever the task has become since
// the caller read it. One rule for every route, so the four cannot disagree
// about what a version is. Faults land on v.
func taskWriteVersion(v *httpx.Validation, version *int64) int64 {
	if version == nil {
		v.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "version is required: send the version your read of the task returned",
			Field:   "version",
		})
		return 0
	}
	// Versions start at 1 and only rise, so anything below names no read.
	if *version < 1 {
		v.Invalid("version", "version must be the version your read of the task returned, a positive integer")
		return 0
	}
	return *version
}

// writeTaskVersionConflict answers a task write whose version is no longer
// the task's: something changed it since the caller read it, and nothing was
// written. No server-side merge: the caller's decision was made against a
// task that no longer exists, so the only honest answer is to read it again.
func writeTaskVersionConflict(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonVersionConflict,
		Message: "this task changed since you read it; refetch it and decide again",
		Field:   "version",
	})
}

// refuseStaleTaskRead answers 409 VERSION_CONFLICT when the handler's own read
// of the task is already past the version the caller sent. It runs straight
// after that read, ahead of every state check, because those checks judge a
// row the caller never saw: a stale write is told it is stale, not that the
// task is closed or claimed. The write's own version predicate is still what
// enforces it; this only answers the common case before anything else runs.
func refuseStaleTaskRead(w http.ResponseWriter, task *domain.Task, version int64) bool {
	if task.Version == version {
		return false
	}
	writeTaskVersionConflict(w)
	return true
}

// handleTaskPatch is the task's field-write path: the lifecycle axis (done,
// dismissed, the in_progress stage) and the wake time. Effects
// that a field write can't express keep their own verb routes — claim and
// delegate reach external systems and spawn runs, and requeue stops the
// task's runs and puts back what a claim changed in Jira or Linear.
//
// Guards apply in two rounds, and the order is deliberate: the body is
// validated first (400 for shape, 422 for a value out of range), and only a
// body that means something is measured against the task's state. So a
// malformed request answers for its own shape even when the task also happens
// to be closed — the task's state is not what is wrong with it. A body naming
// no field is a 400 too: it wrote nothing, and "updated" is the one answer a
// client can't tell from a real write.
//
// Then the state round: a task past the version the body names refuses
// first (409 VERSION_CONFLICT), since every other check would judge a row the
// caller never saw; a closed task refuses everything (409 ALREADY_TERMINAL —
// requeue is how a task re-opens), and the stage status requires the caller's
// own active claim (403). Neither round is the last word on safety; the store
// carries its own predicates, the version among them, because the row can
// change between this handler's read and its write.
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

	req, ok := opTaskUpdate.Decode(w, r)
	if !ok {
		return
	}

	// Two validations because the two fault classes carry different
	// statuses: shape faults are 400 ("you didn't say it right") and range
	// faults 422 ("you said it right and the value can't be one"). Each
	// flushes as a blanket status over everything it accumulated, and the
	// shape pass goes first — a value can't be out of range until it parses.
	var shape, semantic httpx.Validation
	version := taskWriteVersion(&shape, req.Version)
	// Both fields are read before any of their faults are reported, since
	// which fields the body names decides the first two refusals. Their
	// faults then follow those refusals, in field order.
	var statusShape, snoozeShape, snoozeSemantic httpx.Validation
	status, statusState := req.Status.Read(&statusShape, "status")
	snoozeUntil, snoozeState := parseSnoozeUntilPatch(&snoozeShape, &snoozeSemantic, req.SnoozeUntil)
	if statusState == httpx.PatchAbsent && snoozeState == httpx.PatchAbsent {
		shape.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide status or snooze_until (null clears the snooze)",
		})
		shape.Flush(w, http.StatusBadRequest)
		return
	}
	// One axis per request. A body carrying both says two contradictory
	// things about where the task lands — snoozing moves it to 'snoozed', and
	// status names something else — and picking a winner would make the other
	// field a silently-dropped one.
	if statusState != httpx.PatchAbsent && snoozeState != httpx.PatchAbsent {
		shape.Invalid("status", "status and snooze_until can't be set in one request: snoozing moves the task to snoozed by itself")
	}

	addFaults(&shape, &statusShape)
	if statusState == httpx.PatchClear {
		shape.Invalid("status", "status can't be cleared: a task always has one")
	}
	if statusState == httpx.PatchSet && !slices.Contains(patchableTaskStatuses, status) {
		shape.Invalid("status", "status must be one of: "+strings.Join(patchableTaskStatuses, ", "))
	}
	addFaults(&shape, &snoozeShape)
	addFaults(&semantic, &snoozeSemantic)
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
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	if refuseStaleTaskRead(w, task, version) {
		return
	}
	// A closed task is closed: no arm of this route may write over its
	// closed_at / close_reason, snooze it, or move it back to a stage.
	if isTerminalTaskStatus(task.Status) {
		writeTaskTerminal(w, "status")
		return
	}

	var written int64
	var landed bool
	switch {
	case snoozeState == httpx.PatchSet:
		written, landed = s.patchSnooze(w, r, orgID, userID, id, snoozeUntil, version)
	case snoozeState == httpx.PatchClear:
		written, landed = s.patchWake(w, r, orgID, userID, id, version)
	case status == taskStatusInProgress:
		written, landed = s.patchStage(w, r, orgID, userID, id, task, status, version)
	default:
		written, landed = s.patchClose(w, r, orgID, userID, id, status, version)
	}
	if !landed {
		return
	}

	s.writeTaskResource(w, r, opTaskUpdate, orgID, userID, id, written)
}

// addFaults appends src's faults to dst, in src's order.
func addFaults(dst, src *httpx.Validation) {
	for _, item := range src.Items() {
		dst.Add(item)
	}
}

// writtenTaskVersion is the version a task write left, read in the
// transaction that made the write. The write holds the row until that
// transaction commits, so the read sees what the write persisted and nothing
// a later write did. Every write it follows ran under the same RLS as this
// read, and tasks_update's WITH CHECK admits only a row the writer can still
// see, so the row is always there to read.
func writtenTaskVersion(ctx context.Context, tx db.TxStores, orgID, id string) (int64, error) {
	t, err := tx.Tasks.Get(ctx, orgID, id)
	if err != nil {
		return 0, err
	}
	if t == nil {
		return 0, fmt.Errorf("task %s is not readable in the transaction that wrote it", id)
	}
	return t.Version, nil
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
//
// The version is the one the request's own write left, not this read's. The
// read follows the route's cleanup, and a write landing in that time shows in
// the fields but not in the version, so a client that chains its next write
// on this answer is refused rather than writing over a change it did not
// decide against.
func (s *Server) writeTaskResource(w http.ResponseWriter, r *http.Request, op taskWriter, orgID, userID, id string, written int64) bool {
	var updated *domain.Task
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	body := taskToJSON(*updated)
	body.Version = written
	op.Write(w, body)
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
// task under their own RLS within this same request. The version is the one
// the write left, as on writeTaskResource.
func (s *Server) writeTaskResourceSystem(w http.ResponseWriter, r *http.Request, op taskWriter, orgID, userID, id string, written int64) bool {
	var updated *domain.Task
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	body := taskToJSON(*updated)
	body.Version = written
	op.Write(w, body)
	return true
}

// patchableTaskStatuses is the set PATCH accepts. 'queued' isn't among them:
// a task returns to the queue through requeue, which also releases the claim
// and stops whatever the agent was running — a bare status write would strand
// both. 'snoozed' isn't either: it is what setting snooze_until
// means, and accepting it as a status would be a second way to say it, one
// that can't carry the wake time the row needs.
var patchableTaskStatuses = []string{taskStatusInProgress, taskStatusDone, taskStatusDismissed}

// patchSnooze parks a task until the given wake time. Snooze is queue-only
// ("snoozed ↔ both claim cols NULL") and never applies to a closed task, so
// the store's atomic UPDATE carries both predicates and the caller gets a 409
// telling them to requeue first. Returns the version the write left, and
// false when it already wrote an error response.
func (s *Server) patchSnooze(w http.ResponseWriter, r *http.Request, orgID, userID, id string, until time.Time, version int64) (int64, bool) {
	var snoozed bool
	var written int64
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		snoozed, e = tx.Tasks.SnoozeTask(r.Context(), orgID, id, until, version)
		if e != nil || !snoozed {
			return e
		}
		written, e = writtenTaskVersion(r.Context(), tx, orgID, id)
		return e
	}); errors.Is(err, db.ErrTaskVersion) {
		writeTaskVersionConflict(w)
		return 0, false
	} else if err != nil {
		internalError(w, "tasks", err)
		return 0, false
	}
	if !snoozed {
		// Either guard in the store's predicate, at the version the caller
		// read: the task is claimed, or it is closed.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "can't snooze this task; snoozing needs an open, unclaimed task — requeue or complete it first",
		})
		return 0, false
	}
	s.broadcastTaskStatus(orgID)
	return written, true
}

// patchWake clears a wake time early, returning the task to the queue. The
// store guards on status='snoozed' so clearing a wake time a task doesn't have
// is a 409 rather than a silent drag out of whatever lane it was in. Returns
// as patchSnooze does.
func (s *Server) patchWake(w http.ResponseWriter, r *http.Request, orgID, userID, id string, version int64) (int64, bool) {
	var woke bool
	var written int64
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		woke, e = tx.Tasks.ClearSnooze(r.Context(), orgID, id, version)
		if e != nil || !woke {
			return e
		}
		written, e = writtenTaskVersion(r.Context(), tx, orgID, id)
		return e
	}); errors.Is(err, db.ErrTaskVersion) {
		writeTaskVersionConflict(w)
		return 0, false
	} else if err != nil {
		internalError(w, "tasks", err)
		return 0, false
	}
	if !woke {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task isn't snoozed, so there's no wake time to clear",
			Field:   "snooze_until",
		})
		return 0, false
	}
	s.broadcastTaskStatus(orgID)
	return written, true
}

// patchStage re-asserts that the caller is working on this task. It requires
// the caller to hold the user claim: a bot-claimed task is not a person's to
// stage, and an unclaimed one has nobody whose progress this would be. Holding
// a claim already means in progress, so this is idempotent by construction —
// the route stays because "in_progress" is a status a headless caller may
// legitimately PATCH, and the guards are what make it safe. Returns as
// patchSnooze does.
func (s *Server) patchStage(w http.ResponseWriter, r *http.Request, orgID, userID, id string, task *domain.Task, status string, version int64) (int64, bool) {
	if task.ClaimedByUserID != userID {
		forbidden(w, "only the user holding this task's claim can move it to in_progress")
		return 0, false
	}
	var advanced bool
	var written int64
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		advanced, e = tx.Tasks.AdvanceStatusForUser(r.Context(), orgID, id, userID, status, version)
		if e != nil || !advanced {
			return e
		}
		written, e = writtenTaskVersion(r.Context(), tx, orgID, id)
		return e
	}); errors.Is(err, db.ErrTaskVersion) {
		writeTaskVersionConflict(w)
		return 0, false
	} else if err != nil {
		internalError(w, "tasks", err)
		return 0, false
	}
	if !advanced {
		// The pre-read said the caller holds the claim and the task isn't
		// closed, and the version says nothing a person acts on has changed
		// since, so this is a task that went missing underneath us.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task moved since it was read; refetch and retry",
		})
		return 0, false
	}
	s.broadcastTaskStatus(orgID)
	return written, true
}

// patchClose is the terminal arm: dismissed (walked away) or done (resolved).
// It never holds, whatever is working on the task or awaiting the person.
// The store's close carries the terminal predicate itself — the pre-read in
// the handler answers the common case early, but only the predicate rules out
// a close landing between the two, which is what a second close rewriting
// closed_at / close_reason would be. Returns as patchSnooze does.
func (s *Server) patchClose(w http.ResponseWriter, r *http.Request, orgID, userID, id, status string, version int64) (int64, bool) {
	var closed domain.Task
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		closed, e = tx.Tasks.CloseByUser(r.Context(), orgID, id, status, version)
		return e
	})
	if errors.Is(err, db.ErrTaskVersion) {
		// Changed since the pre-read, by a close or anything else a person
		// acts on. Nothing was written.
		writeTaskVersionConflict(w)
		return 0, false
	}
	if errors.Is(err, db.ErrNoSuchTask) {
		// Gone, or closed at the version the caller read, which the pre-read
		// already refused. Nothing was written.
		writeTaskTerminal(w, "status")
		return 0, false
	}
	if err != nil {
		internalError(w, "tasks", err)
		return 0, false
	}
	// The close is the whole write. A run working on the task goes on, its
	// blueprint with it, and the task's deliverables are left as they are: a
	// draft PR stays on GitHub for the person to decide, and a staged review
	// is dismissed only when the pull request it reviews merges or closes.
	//
	// The run is told, and decides under its own instructions whether to go
	// on. Detached from the request: the close committed, and the note is
	// owed whether or not the caller is still connected.
	if s.spawner != nil {
		s.spawner.TellWorkingTaskClosed(context.WithoutCancel(r.Context()), orgID, id)
	}
	s.broadcastTaskStatus(orgID)
	return closed.Version, true
}

// broadcastTaskStatus nudges peer sessions onto a task's new lane. Without it
// a dismissed / completed / snoozed / advanced task stays where it was on
// other browsers until the next user-driven refresh. The frame is a
// payload-free ping, so it is safe under the hub's org-only scope: the board
// refetches through the REST read, which carries the task's visibility.
func (s *Server) broadcastTaskStatus(orgID string) {
	if s.ws == nil {
		return
	}
	s.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{},
	})
}

// taskRequeueRequest is the body of POST /api/tasks/{id}/requeue.
type taskRequeueRequest struct {
	// Version is the task version the caller read (taskJSON.Version).
	Version *int64 `json:"version" doc:"The version the caller's read of the task returned; the requeue is refused when the task has changed since. It is required."`
}

var opTaskRequeue = httpx.NewJSONOp[taskRequeueRequest, taskJSON](httpx.OpSpec{
	Pattern:     "POST /api/tasks/{id}/requeue",
	ID:          "requeue_task",
	Summary:     "Return a task to the queue",
	Description: "Puts the task back in the queue whatever its status, unclaimed, and returns it. It stops every run working on the task and ends its conversations, keeps the deliverables they left, and, where the claim moved a Jira or Linear issue to the claimant, unassigns the issue and moves it back to the status it had before.",
	PathParams:  map[string]string{"id": "The task's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleRequeue puts a task back in the queue, whatever its status, and
// runs the cleanup that goes with it (finalizeRequeue). Used by Board's
// drag-to-Queue gesture, the AgentCard's "Return to queue" button on a
// conversation awaiting approval of its deliverable, and the Cards deck's
// undo toast.
//
// Requeue takes any status, so the version is the one thing it refuses on: a
// requeue decided against a task that has changed since (a stale undo toast,
// a board that missed a refresh) answers 409 VERSION_CONFLICT before anything
// is stopped, ended, or put back in Jira or Linear.
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

	req, ok := opTaskRequeue.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	version := taskWriteVersion(&v, req.Version)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Requeue moves a task back to the queue — a viewer can't (TFAC-447).
	if !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}

	var task *domain.Task
	var requeued bool
	var written int64
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		task, e = tx.Tasks.Get(r.Context(), orgID, id)
		if e != nil || task == nil {
			return e
		}
		requeued, e = tx.Tasks.RequeueTask(r.Context(), orgID, id, version)
		if e != nil || !requeued {
			return e
		}
		written, e = writtenTaskVersion(r.Context(), tx, orgID, id)
		return e
	}); errors.Is(err, db.ErrTaskVersion) {
		writeTaskVersionConflict(w)
		return
	} else if err != nil {
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

	s.writeTaskResource(w, r, opTaskRequeue, orgID, userID, id, written)
}

// finalizeRequeue runs the side-effect cleanup /requeue needs after the task
// status flips back to queued. Three steps, in
// this order:
//
//   - stop: every run still working the task is stopped and the blueprint
//     behind it cancelled. A requeued task is unclaimed and unowned, so an
//     agent left executing against it keeps writing messages and landing
//     deliverables on a task whose columns say nobody owns it. The stop is what
//     makes returning a live run to the queue safe, which is why /requeue
//     takes every status rather than guarding on one.
//
//     The deliverables the stopped runs left are deliberately NOT resolved: a
//     requeued task carries its draft PRs and staged reviews to whoever picks
//     it up next, who pushes to the same PR rather than opening a second.
//
//   - boundary: every conversation the task still had is stamped `requeued`,
//     so nothing resumes into a task the next claimant will open its own
//     conversation on, and the reason a person reads is the gesture that ended
//     it. It follows the stop rather than replacing it — a stop that failed
//     still has to leave a row saying the task moved on.
//
//   - Jira reversal: if the task is Jira-backed and its claim recorded the
//     ticket's state (SourceStatus), unassign it and transition it back,
//     reversing only what the claim changed. Guarded against external
//     mutations: skip if someone else now owns the ticket, or if the ticket
//     has progressed out of the in-progress rule entirely (done, back to
//     pickup, etc.).
//
//   - Linear reversal: the same for a Linear-backed task, with a stricter
//     guard: it skips unless the issue is still assigned to the requester
//     and still in the in-progress rule.
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
	// already committed to requeueing via the surrounding /requeue handler, so
	// bailing on browser close would leave a live agent running
	// against a queued task. WithoutCancel inherits the request's values
	// (claims among them) while breaking the cancel chain.
	cleanupCtx := context.WithoutCancel(r.Context())
	s.stopTaskConversations(cleanupCtx, orgID, userID, taskID, delegate.StopCauseTaskRequeued, "")
	s.endTaskConversations(cleanupCtx, orgID, userID, taskID, domain.EndedRequeued)
	s.revertJiraStateIfApplicable(cleanupCtx, orgID, userID, task)
	s.revertLinearStateIfApplicable(cleanupCtx, orgID, userID, task)
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
			Data:  map[string]any{},
		})
	}
}

// revertJiraStateIfApplicable reverses a Jira-backed task's claim as the task
// goes back to the queue, acting as the requesting user: /requeue reaches it
// through finalizeRequeue. task is the row as it stood before
// the requeue, so its SourceStatus is the state the claim recorded; a task
// with none recorded has nothing to put back. applyJiraUnclaim decides what
// to write.
func (s *Server) revertJiraStateIfApplicable(ctx context.Context, orgID, userID string, task *domain.Task) {
	if task == nil || task.EntitySource != "jira" {
		return
	}
	if task.SourceStatus == nil {
		// A person's claim with nothing recorded: the snapshot named no
		// status at claim time, the stored value does not decode, or the
		// row was claimed by a build that did not record one. Without this
		// line the skip leaves no trace for someone asking why the ticket
		// did not move. A task nobody claimed was never going to move.
		if task.ClaimedByUserID != "" {
			jiraLog.Info("requeue revert: no pre-claim state recorded, leaving the ticket",
				"ticket", task.EntitySourceID, "claimed_by", task.ClaimedByUserID)
		}
		return
	}
	// The reversal undoes the user's own claim, so it acts as that user, not
	// the org service account. Best-effort: the task is already requeued, so
	// a missing or unresolvable credential is logged and skipped rather than
	// failing the response or acting as the bot, and so is Jira turned off by
	// an org admin, where leaving the ticket alone is the switch working.
	jiraUserClient, jerr := s.jiraResolver.ForUser(ctx, orgID, userID)
	if jerr != nil {
		switch {
		case errors.Is(jerr, eventsource.ErrDisabled):
			jiraLog.Info("requeue revert: jira is turned off for the org, leaving the ticket", "ticket", task.EntitySourceID)
		case errors.Is(jerr, jira.ErrNoJiraUserCredential):
			jiraLog.Warn("requeue revert: no jira credential for user, skipping ticket", "user", userID, "ticket", task.EntitySourceID)
		default:
			jiraLog.Warn("requeue revert: resolve user client failed, skipping", "ticket", task.EntitySourceID, "error", jerr)
		}
		return
	}
	// The rule read goes through the app-pool ListForTeam under
	// jira_rules_select RLS, matching the claim path. The user's Jira account
	// id is what tells a ticket that was already theirs from one the claim
	// assigned to them.
	var target jiraTaskTarget
	var accountID string
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		orgSet, e := tx.Orgs.GetSettings(ctx, orgID)
		if e != nil {
			return fmt.Errorf("load org settings: %w", e)
		}
		target = lookupJiraTaskTarget(ctx, tx, orgID, domain.EntityScope("jira", orgSet), task)
		accountID, _, e = tx.Users.GetJiraIdentity(ctx, userID, orgSet.JiraBaseURL)
		return e
	}); err != nil {
		jiraLog.Warn("requeue revert: read rule and identity failed, skipping", "ticket", task.EntitySourceID, "error", err)
		return
	}
	var inProgressMembers []domain.JiraStatusRef
	if target.Rule != nil {
		inProgressMembers = target.Rule.InProgressMembers
	}
	// Detached from the request (see syncJiraClaim): the revert outlives the
	// response, so it uses a background context.
	go applyJiraUnclaim(context.Background(), jiraUserClient, target.IssueRef, task.EntitySourceID, *task.SourceStatus, accountID, inProgressMembers)
}

// applyJiraUnclaim is the write a requeue makes in Jira, as the client's user,
// on the issue issue names (its id; issueKey is the key, for the logs).
// It reverses only what the claim changed, read against prior, the state the
// claim recorded:
//
//   - the ticket is unassigned unless prior shows it was already the user's
//     (accountID) — a claim of a ticket they already held assigned nothing;
//   - it moves back to prior's status unless that status is itself an
//     in-progress member — a claim of a ticket already in progress moved
//     nothing.
//
// It does nothing when the ticket has been reassigned to someone else, or has
// left the in-progress rule since: someone moved it on, and a requeue does not
// reach back and undo that. Membership rather than the canonical status,
// because a ticket moved from "In Progress" to "In Review" is still the
// claimer's work in progress. Every failure is logged rather than returned.
func applyJiraUnclaim(ctx context.Context, c *jira.Client, issue, issueKey string, prior domain.SourceStatus, accountID string, ipMembers []domain.JiraStatusRef) {
	state := c.GetClaimState(ctx, issue)
	if state != nil && !state.AssignedToSelf && !state.Unassigned {
		jiraLog.Warn("requeue guard: reassigned to someone else, skipping", "issue", issueKey)
		return
	}
	if state != nil && len(ipMembers) > 0 && !domain.ContainsStatus(ipMembers, claimStatusRef(state)) {
		jiraLog.Warn("requeue guard: status not in in-progress members, skipping",
			"issue", issueKey, "status", state.StatusName,
			"in_progress_members", domain.JiraStatusNames(ipMembers))
		return
	}

	alreadyMine := prior.AssigneeID != "" && prior.AssigneeID == accountID
	if (state == nil || state.AssignedToSelf) && !alreadyMine {
		if err := c.Unassign(ctx, issue); err != nil {
			jiraLog.Error("failed to unassign on requeue", "issue", issueKey, "error", err)
		}
	}
	priorStatus := domain.JiraStatusRef{ID: prior.StatusID, Name: prior.StatusName}
	if domain.ContainsStatus(ipMembers, priorStatus) {
		jiraLog.Info("requeue guard: the claim found the ticket already in progress, leaving its status", "issue", issueKey, "status", prior.StatusName)
		return
	}
	if err := c.TransitionTo(ctx, issue, jira.Status{ID: prior.StatusID, Name: prior.StatusName}); err != nil {
		jiraLog.Error("failed to transition back on requeue", "issue", issueKey, "status", prior.StatusName, "error", err)
	}
}

// revertLinearStateIfApplicable is revertJiraStateIfApplicable's Linear
// sibling: a requeue of a Linear-backed task reverses what the claim
// changed in Linear, acting as the requesting user. task is the row as it
// stood before the requeue, so its SourceStatus is the state the claim
// recorded; a task with none recorded has nothing to put back.
// applyLinearUnclaim decides what to write.
//
// Best-effort, like the Jira reversal: the task is already queued, so a
// missing credential, rule or identity, or Linear turned off by an org admin,
// is logged and skipped rather than failing the response or acting as the
// org's identity instead.
func (s *Server) revertLinearStateIfApplicable(ctx context.Context, orgID, userID string, task *domain.Task) {
	if task == nil || task.EntitySource != "linear" {
		return
	}
	if task.SourceStatus == nil {
		// See revertJiraStateIfApplicable: only a person's claim is worth a
		// line, since a task nobody claimed was never going to move.
		if task.ClaimedByUserID != "" {
			linearLog.Info("requeue revert: no pre-claim state recorded, leaving the issue",
				"issue", task.EntitySourceID, "claimed_by", task.ClaimedByUserID)
		}
		return
	}
	// The rule and identity first: with no in-progress rule the claim moved
	// nothing to undo, and resolving the user's client can mean an OAuth
	// refresh.
	var target linearSyncTarget
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		target, e = readLinearSyncTarget(ctx, tx, orgID, userID, task)
		return e
	}); err != nil {
		linearLog.Warn("requeue revert: read rule failed, skipping", "issue", task.EntitySourceID, "error", err)
		return
	}
	if target.rule == nil || len(target.rule.InProgressMembers) == 0 {
		linearLog.Warn("requeue revert: no in_progress rule for Linear team, skipping", "issue", target.identifier)
		return
	}
	if target.linearUserID == "" {
		linearLog.Warn("requeue revert: no Linear identity stored for user, skipping", "issue", target.identifier, "user", userID)
		return
	}
	if target.issueID == "" {
		linearLog.Warn("requeue revert: the task's entity names no Linear issue id, skipping", "issue", target.identifier)
		return
	}
	linearUserClient, err := s.linearResolver.ForUser(ctx, orgID, userID)
	if err != nil {
		switch {
		case errors.Is(err, eventsource.ErrDisabled):
			linearLog.Info("requeue revert: linear is turned off for the org, leaving the issue", "issue", target.identifier)
		case errors.Is(err, linear.ErrNoLinearUserCredential):
			linearLog.Warn("requeue revert: no linear credential for user, skipping", "user", userID, "issue", target.identifier)
		default:
			linearLog.Warn("requeue revert: resolve user client failed, skipping", "issue", target.identifier, "error", err)
		}
		return
	}
	// Detached from the request, like the Jira reversal.
	go applyLinearUnclaim(context.Background(), linearUserClient, target.issueID, target.identifier, target.linearUserID,
		*task.SourceStatus, target.rule.InProgressMembers)
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
func parseSnoozeUntilPatch(shape, semantic *httpx.Validation, field httpx.Patch[string]) (time.Time, httpx.PatchState) {
	value, state := field.Read(shape, "snooze_until")
	if state != httpx.PatchSet {
		return time.Time{}, state
	}
	until, err := time.Parse(time.RFC3339, value)
	if err != nil {
		shape.Invalid("snooze_until", "snooze_until must be an RFC3339 timestamp, or null")
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

func (s *Server) registerTaskRoutes() {
	// Find-or-create the task at a station. The Factory's drop gesture is this
	// plus POST /api/tasks/{id}/delegate: the task is the row, delegating is
	// what you then do to it, so the in-between state — task resolved, run not
	// started — is addressable rather than a partial success buried in one
	// call's response.
	s.op(opTaskCreate, s.handleTaskCreate)
	// The one tasks list read: every filtered view of the queue goes through
	// it, so two spellings of the same read cannot drift into two different
	// hidden filters. As a POST it mounts behind the CSRF check, which is
	// deliberate for a body-carrying read — see handleTaskList.
	s.op(opTaskList, s.handleTaskList)
	// The lane's own event-type set, beside the list and under the same
	// filters minus the reader's own — the chips a lane offers are about the
	// lane, not about the page a filtered query happened to return. A POST
	// for the same reason the list is: the filter is a structured body.
	s.op(opTaskFacets, s.handleTaskFacets)
	s.op(opTaskGet, s.handleTaskGet)
	// The task's field-write path: the lifecycle axis and the wake time are
	// columns, so they are PATCHed rather than given a route per value — a
	// route per value is how one gesture grows arms with different required
	// fields, different authz and different response shapes. Each verb below
	// earns its own route by carrying an effect a field write cannot express:
	// an external Jira write, a spawned run, a stopped one.
	s.op(opTaskUpdate, s.handleTaskPatch)
	s.op(opTaskClaim, s.handleTaskClaim)
	s.op(opTaskDelegate, s.handleTaskDelegate)
	s.op(opTaskRequeue, s.handleRequeue)
}
