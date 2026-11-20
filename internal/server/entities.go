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

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The entity and event reads: POST /api/entities/list, GET
// /api/entities/{id}, and POST /api/events/list.
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
	Number    int    `json:"number"`
	Repo      string `json:"repo"`
	Author    string `json:"author"`
	Draft     bool   `json:"draft"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// entityJSON is the entity resource. Every key is always present: a value a
// row does not have is null, never an absent key, so a client reads one
// shape for every source.
type entityJSON struct {
	ID        string     `json:"id"`
	Source    string     `json:"source"`
	SourceID  string     `json:"source_id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	URL       string     `json:"url"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	// RepositoryID is a GitHub entity's repository registry row, the id a
	// client groups pull requests by: a rename moves the repository's name
	// and leaves the row. Null for every other source.
	RepositoryID *string `json:"repository_id"`
	// LatestEventType and LatestEventAt are the entity's newest recorded
	// event, its type and detection time. Null when it has none.
	LatestEventType *string    `json:"latest_event_type"`
	LatestEventAt   *time.Time `json:"latest_event_at"`
	// Mine is whether the viewer wrote it: a pull request whose author is the
	// viewer's GitHub identity on the org's host. False for every other kind,
	// for which TF holds no author it can compare.
	Mine bool `json:"mine"`
	// PullRequest is a pull request's fields from its last poll. Null for
	// every other kind, and for a pull request no poll has read yet.
	PullRequest *entityPullRequestJSON `json:"pull_request"`
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
	IDs []string `json:"ids"`
	// Sources keeps the entities of these sources (entitySources).
	Sources []string `json:"sources"`
	// Kinds keeps the entities of these kinds (domain.EntityKinds).
	Kinds []string `json:"kinds"`
	// States keeps the entities in these states (domain.EntityStates).
	States []string `json:"states"`
	// ClosedSince (RFC3339) drops an entity that closed before it; an entity
	// that has not closed passes. With no states filter it is the active set
	// plus whatever closed since then, in one read.
	ClosedSince string `json:"closed_since"`
	// TeamIDs narrows the tracked set to these teams'. Empty is every team
	// of the viewer's, and a team the viewer is not on narrows to nothing.
	TeamIDs []string `json:"team_ids"`

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

// handleEntityList serves POST /api/entities/list: the entities the viewer
// may read, newest first, each with its latest event.
func (s *Server) handleEntityList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req entityListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
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
		if _, err := uuid.Parse(id); err != nil {
			v.Invalid("team_ids", fmt.Sprintf("team id %q is not a valid team id", id))
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
	httpx.WriteList(w, page, out, total)
}

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
	writeJSON(w, http.StatusOK, toEntityJSON(items[0], me))
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
	ID         string     `json:"id"`
	EntityID   string     `json:"entity_id"`
	EventType  string     `json:"event_type"`
	DedupKey   string     `json:"dedup_key"`
	OccurredAt *time.Time `json:"occurred_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// eventListRequest is the body of POST /api/events/list. Every field is
// optional and each one narrows.
type eventListRequest struct {
	// EntityIDs keeps the events of these entities, each in canonical form,
	// at most maxEntityListIDs.
	EntityIDs []string `json:"entity_ids"`
	// Since (RFC3339) keeps the events recorded at or after it.
	Since string `json:"since"`

	httpx.PageRequest
}

type eventListFilterKey struct {
	EntityIDs []string `json:"entity_ids"`
	Since     string   `json:"since"`
}

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

	var req eventListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	filter := db.EventListFilter{EntityIDs: canonicalIDList(&v, "entity_ids", "an entity", req.EntityIDs)}
	var sinceKey string
	filter.Since, sinceKey = parseTaskWindow(&v, "since", req.Since)
	page := httpx.ResolveKeysetPage(&v, req.PageRequest, httpx.FilterFingerprint(eventListFilterKey{
		EntityIDs: filter.EntityIDs, Since: sinceKey,
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
		ev := row.Event
		items[i] = eventJSON{ID: ev.ID, EventType: ev.EventType, DedupKey: ev.DedupKey, CreatedAt: ev.CreatedAt}
		if ev.EntityID != nil {
			items[i].EntityID = *ev.EntityID
		}
		if !ev.OccurredAt.IsZero() {
			at := ev.OccurredAt
			items[i].OccurredAt = &at
		}
	}
	httpx.WriteListKeyset(w, page, items, total, nextKey)
}

func (s *Server) registerEntityRoutes() {
	// The entity resource and its event log, under the tracked-set read rule.
	// Lists are POST for their bodies, and so register as mutating for CSRF.
	s.apiMutating("POST /api/entities/list", s.handleEntityList)
	s.api("GET /api/entities/{id}", s.handleEntityGet)
	s.apiMutating("POST /api/events/list", s.handleEventList)
}
