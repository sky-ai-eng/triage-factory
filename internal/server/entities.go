package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The entity and event reads: POST /api/entities/list, GET
// /api/entities/{id}, POST /api/events/list, and GET /api/events/{id}.
//
// An entity is listed to a viewer when one of their teams tracks its unit (a
// repository, a Jira project, a Linear team, a Slack channel) — the standing
// rule for an entity read that surfaces untasked entities — and an event when
// its entity is. Local mode is N=1 and lists every entity.
// --------------------------------------------------------------------

// maxEntityListIDs caps the ids filter of the entity and event lists at one
// full page, so a read that names its rows and asks for that many gets every
// one on its first page.
const maxEntityListIDs = httpx.MaxPageSize

// entitySources is the vocabulary of the entity list's sources filter: every
// source an entity is stored under. A system event has no entity, so
// "system" names none.
func entitySources() []string {
	return slices.DeleteFunc(domain.EventSources(), func(s string) bool { return s == "system" })
}

// entityPullRequestJSON is a pull request entity's fields, read from its
// snapshot.
type entityPullRequestJSON struct {
	Number    int    `json:"number" doc:"The pull request's number in its repository."`
	Repo      string `json:"repo" doc:"The repository, as owner/name."`
	Author    string `json:"author" doc:"The GitHub login of the pull request's author."`
	Draft     bool   `json:"draft" doc:"Whether the pull request is a draft."`
	Additions int    `json:"additions" doc:"How many lines the pull request adds."`
	Deletions int    `json:"deletions" doc:"How many lines the pull request removes."`
}

// entityJSON is the entity resource. Every key is always present: a value a
// row does not have is null, never an absent key, so a client reads one
// shape for every source.
type entityJSON struct {
	ID        string     `json:"id" doc:"The entity's id."`
	Source    string     `json:"source" doc:"The system the entity comes from: github, jira, linear, or slack."`
	SourceID  string     `json:"source_id" doc:"The entity's key in its source, such as owner/repo#42 for a pull request or PROJ-123 for a Jira issue."`
	Kind      string     `json:"kind" doc:"The kind of entity: pr, issue, vulnerable_package, thread, or message."`
	Title     string     `json:"title" doc:"The entity's title."`
	URL       string     `json:"url" doc:"The entity's page in its source."`
	State     string     `json:"state" doc:"Whether the entity is active or closed."`
	CreatedAt time.Time  `json:"created_at" doc:"When the entity was first recorded, as an RFC 3339 timestamp."`
	ClosedAt  *time.Time `json:"closed_at" doc:"When the entity closed, as an RFC 3339 timestamp, or null while it is active."`
	// RepositoryID is a GitHub entity's repository registry row, the id a
	// client groups pull requests by: a rename moves the repository's name
	// and leaves the row. Null for every other source.
	RepositoryID *string `json:"repository_id" doc:"The id of a GitHub entity's repository, which a rename of the repository does not change, or null for an entity from any other source."`
	// LatestEventType and LatestEventAt are the entity's newest recorded
	// event, its type and detection time. Null when it has none.
	LatestEventType *string    `json:"latest_event_type" doc:"The type of the entity's newest recorded event, or null when it has none."`
	LatestEventAt   *time.Time `json:"latest_event_at" doc:"When the entity's newest event was recorded, as an RFC 3339 timestamp, or null when it has none."`
	// Mine is whether the viewer wrote it: a pull request whose author is the
	// viewer's GitHub identity on the org's host. False for every other kind,
	// for which TF holds no author it can compare.
	Mine bool `json:"mine" doc:"Whether the entity is a pull request the caller's GitHub identity on the organization's host wrote; false for every other kind of entity."`
	// PullRequest is a pull request's fields from its last poll. Null for
	// every other kind, and for a pull request no poll has read yet.
	PullRequest *entityPullRequestJSON `json:"pull_request" doc:"A pull request's fields as its last poll read them, or null for every other kind of entity and for a pull request no poll has read yet."`
}

// toEntityJSON projects a listed entity onto the wire, comparing a pull
// request's author with the viewer's GitHub account for mine.
func toEntityJSON(item domain.ListedEntity, me domain.GitHubAccount) entityJSON {
	e := item.Entity
	out := entityJSON{
		ID: e.ID, Source: e.Source, SourceID: e.SourceID, Kind: e.Kind,
		Title: e.Title, URL: e.URL, State: e.State,
		CreatedAt: e.CreatedAt.UTC(), ClosedAt: e.ClosedAt,
		LatestEventAt: item.LatestEventAt,
	}
	if e.RepositoryID != "" {
		id := e.RepositoryID
		out.RepositoryID = &id
	}
	if item.LatestEventType != "" {
		et := item.LatestEventType
		out.LatestEventType = &et
	}
	if e.Source == domain.ProviderGitHub && e.Kind == domain.EntityKindPullRequest && e.SnapshotJSON != "" {
		var snap domain.PRSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			serverLog.Warn("entity has malformed snapshot_json", "entity", e.ID, "error", err)
		} else if snap.Number != 0 {
			out.PullRequest = &entityPullRequestJSON{
				Number: snap.Number, Repo: snap.Repo, Author: snap.Author, Draft: snap.IsDraft,
				Additions: snap.Additions, Deletions: snap.Deletions,
			}
			out.Mine = me.Is(snap.Author, snap.AuthorID)
		}
	}
	return out
}

// entityListRequest is the body of POST /api/entities/list. Every field is
// optional and each one narrows; `{}` is every entity the viewer may read.
type entityListRequest struct {
	// IDs keeps the entities with these ids, each in canonical form, at most
	// maxEntityListIDs.
	IDs []string `json:"ids" doc:"Ids of the entities to include, at most 200, each a UUID in lowercase hyphenated form; empty includes every entity."`
	// Sources keeps the entities of these sources (entitySources).
	Sources []string `json:"sources" doc:"Sources whose entities to include, from github, jira, linear, and slack; empty includes every source."`
	// Kinds keeps the entities of these kinds (domain.EntityKinds).
	Kinds []string `json:"kinds" doc:"Kinds of entity to include, from issue, message, pr, thread, and vulnerable_package; empty includes every kind."`
	// States keeps the entities in these states (domain.EntityStates).
	States []string `json:"states" doc:"States to include, from active and closed; empty includes both."`
	// ClosedSince (RFC3339) drops an entity that closed before it; an entity
	// that has not closed passes. With no states filter it is the active set
	// plus whatever closed since then, in one read.
	ClosedSince string `json:"closed_since" doc:"An RFC 3339 timestamp that leaves out entities that closed before it; an entity that has not closed is included."`
	// TeamIDs narrows the tracked set to these teams'. Empty is every team
	// of the viewer's, and a team the viewer is not on narrows to nothing.
	TeamIDs []string `json:"team_ids" doc:"Ids of the teams, at most 100, whose tracked repositories, projects, teams, and channels limit the result; empty uses every team the caller belongs to, and a team the caller is not on matches nothing."`

	httpx.PageRequest
}

// entityListFilterKey is the canonicalized filter set the page token is
// fingerprinted against.
type entityListFilterKey struct {
	IDs         []string `json:"ids"`
	Sources     []string `json:"sources"`
	Kinds       []string `json:"kinds"`
	States      []string `json:"states"`
	ClosedSince string   `json:"closed_since"`
	TeamIDs     []string `json:"team_ids"`
}

// canonicalIDList validates a list body's filter of ids, each a uuid in
// canonical form (httpx.IsCanonicalID) and at most maxEntityListIDs distinct
// ones, and returns it canonicalized. noun names what the ids address.
func canonicalIDList(v *httpx.Validation, field, noun string, raw []string) []string {
	for i, id := range raw {
		if !httpx.IsCanonicalID(id) {
			v.Invalid(fmt.Sprintf("%s[%d]", field, i), fmt.Sprintf("%q is not %s id in canonical form", id, noun))
		}
	}
	ids := httpx.CanonicalStrings(raw)
	if len(ids) > maxEntityListIDs {
		v.OutOfRange(field, fmt.Sprintf("%s accepts at most %d ids per request; got %d", field, maxEntityListIDs, len(ids)))
	}
	return ids
}

// vocabularyList validates a list body's filter against its closed
// vocabulary and returns it canonicalized. An unknown value is a client
// fault, never an empty page.
func vocabularyList(v *httpx.Validation, field, noun string, raw, vocabulary []string) []string {
	values := httpx.CanonicalStrings(raw)
	for _, value := range values {
		if !slices.Contains(vocabulary, value) {
			v.Invalid(field, fmt.Sprintf("unknown %s %q; must be one of: %s", noun, value, strings.Join(vocabulary, ", ")))
		}
	}
	return values
}

var opEntityList = httpx.NewListOp[entityListRequest, entityJSON](httpx.OpSpec{
	Pattern:     "POST /api/entities/list",
	ID:          "list_entities",
	Summary:     "List entities",
	Description: "Returns one page of the entities the caller may read, newest first, narrowed by the body's filters: the pull requests, issues, vulnerable packages, and Slack threads and messages in the repositories, projects, teams, and channels the caller's teams track. Each entity carries its newest event's type and time.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleEntityList serves POST /api/entities/list: the entities the viewer
// may read, newest first, each with its latest event.
func (s *Server) handleEntityList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	req, ok := opEntityList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	filter := db.EntityListFilter{
		IDs:     canonicalIDList(&v, "ids", "an entity", req.IDs),
		Sources: vocabularyList(&v, "sources", "source", req.Sources, entitySources()),
		Kinds:   vocabularyList(&v, "kinds", "entity kind", req.Kinds, domain.EntityKinds()),
		States:  vocabularyList(&v, "states", "state", req.States, domain.EntityStates()),
		TeamIDs: httpx.CanonicalStrings(req.TeamIDs),
	}
	for _, id := range filter.TeamIDs {
		// Canonical, not merely parseable: SQLite compares the text, and
		// Postgres refuses some spellings uuid.Parse takes (urn:uuid:…).
		if !httpx.IsCanonicalID(id) {
			v.Invalid("team_ids", fmt.Sprintf("%q is not a team id in canonical form", id))
		}
	}
	if len(filter.TeamIDs) > maxBatchTeamIDs {
		v.OutOfRange("team_ids", fmt.Sprintf("team_ids accepts at most %d ids per request; got %d", maxBatchTeamIDs, len(filter.TeamIDs)))
	}
	var closedSinceKey string
	filter.ClosedSince, closedSinceKey = parseTaskWindow(&v, "closed_since", req.ClosedSince)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(entityListFilterKey{
		IDs: filter.IDs, Sources: filter.Sources, Kinds: filter.Kinds, States: filter.States,
		ClosedSince: closedSinceKey, TeamIDs: filter.TeamIDs,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	items, total, me, err := s.listEntities(r, orgID, userID, filter,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
	if err != nil {
		internalError(w, "entities", err)
		return
	}
	out := make([]entityJSON, len(items))
	for i, it := range items {
		out[i] = toEntityJSON(it, me)
	}
	opEntityList.Write(w, page, out, total)
}

var opEntityGet = httpx.NewJSONOp[httpx.NoBody, entityJSON](httpx.OpSpec{
	Pattern:     "GET /api/entities/{id}",
	ID:          "get_entity",
	Summary:     "Get an entity",
	Description: "Returns one entity the caller may read, in the shape the entity list returns it.",
	PathParams:  map[string]string{"id": "The entity's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleEntityGet serves GET /api/entities/{id}. An entity the viewer may not
// read is 404, like one that does not exist: it is in none of their lists.
func (s *Server) handleEntityGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	if !httpx.IsCanonicalID(id) {
		notFound(w, "entity")
		return
	}
	items, _, me, err := s.listEntities(r, orgID, userID, db.EntityListFilter{IDs: []string{id}}, db.ListOpts{Limit: 1})
	if err != nil {
		internalError(w, "entities", err)
		return
	}
	if len(items) == 0 {
		notFound(w, "entity")
		return
	}
	opEntityGet.Write(w, toEntityJSON(items[0], me))
}

// listEntities reads one window of the entity list under the viewer's claims,
// with the viewer's GitHub account mine compares authors against. A viewer
// with no GitHub identity on the org's host, or an org with no GitHub host,
// is the zero account, which is nobody's author.
func (s *Server) listEntities(r *http.Request, orgID, userID string, f db.EntityListFilter, opts db.ListOpts) ([]domain.ListedEntity, int, domain.GitHubAccount, error) {
	var items []domain.ListedEntity
	var total int
	var me domain.GitHubAccount
	err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		items, total, e = tx.Entities.List(r.Context(), orgID, f, opts)
		if e != nil || opts.CountOnly {
			return e
		}
		settings, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		host, ok := resolveGitHubHost(settings.GitHubBaseURL)
		if !ok {
			return nil
		}
		identity, e := tx.Users.GetGitHubIdentity(r.Context(), userID, host)
		if e != nil {
			return e
		}
		me = domain.GitHubAccountOf(identity)
		return nil
	})
	return items, total, me, err
}

// eventJSON is the event resource as the event list serves it. OccurredAt is
// the source's own time for the event (a commit's, a check run's completion,
// a review's submission) and null when the source reported none; CreatedAt is
// when TF recorded it.
type eventJSON struct {
	ID         string     `json:"id" doc:"The event's id."`
	EntityID   string     `json:"entity_id" doc:"The id of the entity the event happened on."`
	EventType  string     `json:"event_type" doc:"The event's type, such as github:pr:ci_check_failed."`
	DedupKey   string     `json:"dedup_key" doc:"The value that tells apart events of one type on one entity, such as a label name, or an empty string for most event types."`
	OccurredAt *time.Time `json:"occurred_at" doc:"When the event happened in its source, as an RFC 3339 timestamp, such as a commit's or a review's time, or null when the source reported none."`
	CreatedAt  time.Time  `json:"created_at" doc:"When the event was recorded, as an RFC 3339 timestamp."`
}

// eventListRequest is the body of POST /api/events/list. Every field is
// optional and each one narrows.
type eventListRequest struct {
	// EntityIDs keeps the events of these entities, each in canonical form,
	// at most maxEntityListIDs.
	EntityIDs []string `json:"entity_ids" doc:"Ids of the entities whose events to include, at most 200, each a UUID in lowercase hyphenated form; empty includes the events of every entity the caller may read."`
	// Since (RFC3339) keeps the events recorded at or after it.
	Since string `json:"since" doc:"An RFC 3339 timestamp that limits the result to events recorded at or after it."`
	// PerEntity keeps each entity's newest per_entity recorded events, 1 to
	// httpx.MaxPageSize, so a read naming many entities gets a tail of each
	// however busy one of them is. Absent is no cap.
	PerEntity *int `json:"per_entity" doc:"The most events to include for each entity, its newest, from 1 to 200; absent includes every event."`

	httpx.PageRequest
}

type eventListFilterKey struct {
	EntityIDs []string `json:"entity_ids"`
	Since     string   `json:"since"`
	PerEntity int      `json:"per_entity"`
}

var opEventList = httpx.NewListOp[eventListRequest, eventJSON](httpx.OpSpec{
	Pattern:     "POST /api/events/list",
	ID:          "list_events",
	Summary:     "List events",
	Description: "Returns one page of the events recorded on the entities the caller may read, newest recorded first, narrowed by the body's filters. A page_token resumes after the last event of the page that returned it, so a walk neither repeats nor skips an event while new ones are recorded.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleEventList serves POST /api/events/list: the events of the entities
// the viewer may read, newest recorded first. It pages by keyset on the
// recording order, so a walk neither repeats nor skips an event while events
// keep arriving.
func (s *Server) handleEventList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	req, ok := opEventList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	filter := db.EventListFilter{EntityIDs: canonicalIDList(&v, "entity_ids", "an entity", req.EntityIDs)}
	var sinceKey string
	filter.Since, sinceKey = parseTaskWindow(&v, "since", req.Since)
	if req.PerEntity != nil {
		if *req.PerEntity < 1 || *req.PerEntity > httpx.MaxPageSize {
			v.OutOfRange("per_entity", fmt.Sprintf("per_entity must be between 1 and %d; got %d", httpx.MaxPageSize, *req.PerEntity))
		}
		filter.PerEntity = *req.PerEntity
	}
	page := httpx.ResolveKeysetPage(&v, req.PageRequest, httpx.FilterFingerprint(eventListFilterKey{
		EntityIDs: filter.EntityIDs, Since: sinceKey, PerEntity: filter.PerEntity,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// One row past the window answers whether a page follows, which a keyset
	// page cannot derive from its total.
	opts := db.ListOpts{Limit: page.Limit, After: page.After, CountOnly: page.CountOnly}
	if opts.Limit > 0 {
		opts.Limit++
	}
	var rows []db.ListedEvent
	var total int
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rows, total, e = tx.Events.List(r.Context(), orgID, filter, opts)
		return e
	}); err != nil {
		if errors.Is(err, db.ErrBadPageCursor) {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidParam,
				Message: "page_token does not address this ordering; restart from the first page",
				Field:   "page_token",
			})
			return
		}
		internalError(w, "events", err)
		return
	}
	var nextKey []string
	if page.Limit > 0 && len(rows) > page.Limit {
		rows = rows[:page.Limit]
		nextKey = []string{strconv.FormatInt(rows[len(rows)-1].Position, 10)}
	}
	items := make([]eventJSON, len(rows))
	for i, row := range rows {
		items[i] = toEventJSON(row.Event)
	}
	opEventList.WriteKeyset(w, page, items, total, nextKey)
}

func toEventJSON(ev domain.Event) eventJSON {
	out := eventJSON{ID: ev.ID, EventType: ev.EventType, DedupKey: ev.DedupKey, CreatedAt: ev.CreatedAt}
	if ev.EntityID != nil {
		out.EntityID = *ev.EntityID
	}
	if !ev.OccurredAt.IsZero() {
		at := ev.OccurredAt
		out.OccurredAt = &at
	}
	return out
}

var opEventGet = httpx.NewJSONOp[httpx.NoBody, eventJSON](httpx.OpSpec{
	Pattern:     "GET /api/events/{id}",
	ID:          "get_event",
	Summary:     "Get an event",
	Description: "Returns one event recorded on an entity the caller may read, in the shape the event list returns it.",
	PathParams:  map[string]string{"id": "The event's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleEventGet serves GET /api/events/{id}. An event the viewer may not
// read, including a system event, which has no entity, is 404 like one that
// does not exist: it is in none of their lists.
func (s *Server) handleEventGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	if !httpx.IsCanonicalID(id) {
		notFound(w, "event")
		return
	}
	var rows []db.ListedEvent
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rows, _, e = tx.Events.List(r.Context(), orgID, db.EventListFilter{IDs: []string{id}}, db.ListOpts{Limit: 1})
		return e
	}); err != nil {
		internalError(w, "events", err)
		return
	}
	if len(rows) == 0 {
		notFound(w, "event")
		return
	}
	opEventGet.Write(w, toEventJSON(rows[0].Event))
}

func (s *Server) registerEntityRoutes() {
	// The entity resource and its event log, under the tracked-set read rule.
	// Lists are POST for their bodies, and so register as mutating for CSRF.
	s.op(opEntityList, s.handleEntityList)
	s.op(opEntityGet, s.handleEntityGet)
	s.op(opEventList, s.handleEventList)
	s.op(opEventGet, s.handleEventGet)
}
