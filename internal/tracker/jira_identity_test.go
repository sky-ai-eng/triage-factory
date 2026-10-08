package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The identity tests: a Jira issue's entity is the row carrying its numeric id
// on the org's site, so a new key is a rename of that row and a new site is a
// new namespace.

const jiraSite = "https://jira.example.com"

// fakeJiraIssue is one issue on fakeJira. Keys lists every key it has had,
// current last: Jira resolves an old key to the issue under its current one.
type fakeJiraIssue struct {
	ID        string
	Keys      []string
	ProjectID string
	Status    string
	Assignee  string // display name; "" is unassigned
	Summary   string
	Updated   string
	Gone      bool // 404 on every read, absent from every search
}

func (is *fakeJiraIssue) key() string { return is.Keys[len(is.Keys)-1] }

func (is *fakeJiraIssue) json() map[string]any {
	fields := map[string]any{
		"summary": is.Summary,
		"status":  map[string]string{"id": "st-" + is.Status, "name": is.Status},
		"project": map[string]string{"id": is.ProjectID, "key": extractProject(is.key())},
		"updated": is.Updated,
	}
	if is.Assignee != "" {
		fields["assignee"] = map[string]string{"displayName": is.Assignee, "accountId": "acc-" + strings.ToLower(is.Assignee)}
	}
	return map[string]any{"id": is.ID, "key": is.key(), "fields": fields}
}

// fakeJira answers the reads a Jira cycle makes from a set of issues it
// mutates between cycles: discovery's project queries, the refresh's
// `id IN` / `key IN` batches, and the confirmation's per-issue GET. It records
// every search and GET, so a test can say which reads a cycle made.
type fakeJira struct {
	mu       sync.Mutex
	issues   []*fakeJiraIssue
	searches []string
	gets     []string
	srv      *httptest.Server
}

var (
	jqlListRe    = regexp.MustCompile(`^(id|key) IN \((.*)\)$`)
	jqlProjectRe = regexp.MustCompile(`^project = "([^"]+)"`)
)

func newFakeJira(t *testing.T, issues ...*fakeJiraIssue) *fakeJira {
	t.Helper()
	f := &fakeJira{issues: issues}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJira) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/search"):
		var req struct {
			JQL string `json:"jql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.searches = append(f.searches, req.JQL)
		var out []map[string]any
		for _, is := range f.issues {
			if !is.Gone && f.matches(req.JQL, is) {
				out = append(out, is.json())
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": out, "total": len(out)})
	case strings.Contains(r.URL.Path, "/issue/"):
		idOrKey := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.gets = append(f.gets, idOrKey)
		for _, is := range f.issues {
			if !is.Gone && (is.ID == idOrKey || slices.Contains(is.Keys, idOrKey)) {
				_ = json.NewEncoder(w).Encode(is.json())
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`))
	default:
		http.NotFound(w, r)
	}
}

// matches is the slice of JQL a cycle sends. An id or key list matches an
// issue by id, or by any key it has had. A discovery query matches the issues
// currently in its project that are not done: the pickup query the unassigned
// ones, the assigned-to-current-user query the assigned ones.
func (f *fakeJira) matches(jql string, is *fakeJiraIssue) bool {
	if m := jqlListRe.FindStringSubmatch(jql); m != nil {
		for _, v := range strings.Split(m[2], ", ") {
			if (m[1] == "id" && v == is.ID) || (m[1] == "key" && slices.Contains(is.Keys, v)) {
				return true
			}
		}
		return false
	}
	if m := jqlProjectRe.FindStringSubmatch(jql); m != nil {
		if extractProject(is.key()) != m[1] || is.Status == "Done" {
			return false
		}
		if strings.Contains(jql, "currentUser()") {
			return is.Assignee != ""
		}
		return is.Assignee == ""
	}
	return false
}

func (f *fakeJira) reads() (searches, gets []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	searches, gets = slices.Clone(f.searches), slices.Clone(f.gets)
	f.searches, f.gets = nil, nil
	return searches, gets
}

func (f *fakeJira) update(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// jiraIdentityFixture is a migrated database, a tracker over it, and the fake
// Jira a cycle reads from.
type jiraIdentityFixture struct {
	db     *sql.DB
	stores db.Stores
	jira   *fakeJira
	client *jiraclient.Client
}

func newJiraIdentityFixture(t *testing.T, issues ...*fakeJiraIssue) *jiraIdentityFixture {
	t.Helper()
	database := newMigratedSQLite(t)
	f := newFakeJira(t, issues...)
	return &jiraIdentityFixture{
		db:     database,
		stores: sqlitestore.New(database),
		jira:   f,
		client: jiraclient.NewClient(jiraclient.DataCenterPAT(f.srv.URL, "pat")),
	}
}

// cycle runs one Jira cycle on site against rules, and returns the events it
// published, poll-complete aside.
func (fx *jiraIdentityFixture) cycle(t *testing.T, site string, rules JiraRules) []domain.Event {
	t.Helper()
	pub := &recordingPublisher{}
	tr := New(fx.db, pub, fx.stores.Tasks, fx.stores.Entities, fx.stores.Repos, fx.stores.EventQueue, runmode.LocalDefaultOrgID)
	if _, err := tr.RefreshJira(context.Background(), site, fx.client, site, rules); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	return pub.nonSystemEvents()
}

func (fx *jiraIdentityFixture) entity(t *testing.T, site, key string) *domain.Entity {
	t.Helper()
	e, err := fx.stores.Entities.GetBySourceSystem(context.Background(), runmode.LocalDefaultOrgID, "jira", site, key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return e
}

func (fx *jiraIdentityFixture) seedTask(t *testing.T, entityID string) string {
	t.Helper()
	ctx := context.Background()
	eventID, err := fx.stores.Events.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.Event{
		EntityID: &entityID, EventType: domain.EventJiraIssueAssigned, MetadataJSON: "{}",
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := fx.stores.Tasks.FindOrCreate(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entityID, domain.EventJiraIssueAssigned, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task.ID
}

func (fx *jiraIdentityFixture) countEntities(t *testing.T) int {
	t.Helper()
	var n int
	if err := fx.db.QueryRow(`SELECT COUNT(*) FROM entities WHERE source = 'jira'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func engAndOps() JiraRules {
	return JiraRules{
		{Key: "ENG", PickupMembers: jiraRefs("To Do"), DoneMembers: jiraRefs("Done")},
		{Key: "OPS", PickupMembers: jiraRefs("To Do"), DoneMembers: jiraRefs("Done")},
	}
}

func engOnly() JiraRules { return engAndOps()[:1] }

func decodeMeta[T any](t *testing.T, evt domain.Event) T {
	t.Helper()
	var m T
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
		t.Fatalf("decode %s metadata: %v", evt.EventType, err)
	}
	return m
}

// TestRefreshJira_MoveBetweenConfiguredProjects: an issue moved from one
// configured project to another is the same entity under its new key. Diffing
// never pauses, its tasks stay on it, its url and its artifacts' targets follow
// it, and the move is key_changed, emitted before anything else the cycle
// diffs — never unreachable, never a second entity.
func TestRefreshJira_MoveBetweenConfiguredProjects(t *testing.T) {
	is := &fakeJiraIssue{ID: "10001", Keys: []string{"ENG-1"}, ProjectID: "100", Status: "In Progress", Assignee: "Alice", Summary: "Fix it", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, is)
	if evts := fx.cycle(t, jiraSite, engAndOps()); len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueAssigned {
		t.Fatalf("first cycle = %v, want the assignment", eventTypes(evts))
	}
	before := fx.entity(t, jiraSite, "ENG-1")
	if before == nil || before.ExternalID != "10001" {
		t.Fatalf("entity = %+v, want it created carrying the issue id", before)
	}
	taskID := fx.seedTask(t, before.ID)
	art, err := fx.stores.Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, domain.Artifact{
		TeamID: runmode.LocalDefaultTeamID, Provider: domain.ArtifactProviderJira, Kind: domain.ArtifactKindIssue,
		Target: "ENG-1", ExternalID: "10001", State: domain.ArtifactStateIssueUpdated,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindIssue, domain.JiraIssueResource(jiraSite, "10001"), ""),
	})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	fx.jira.update(func() {
		is.Keys, is.ProjectID, is.Status, is.Updated = append(is.Keys, "OPS-7"), "200", "Review", "2026-10-01T11:00:00.000+0000"
	})
	fx.jira.reads()
	evts := fx.cycle(t, jiraSite, engAndOps())
	got := eventTypes(evts)
	if len(got) == 0 || got[0] != domain.EventJiraIssueKeyChanged {
		t.Fatalf("events = %v, want key_changed first", got)
	}
	if !slices.Contains(got, domain.EventJiraIssueStatusChanged) || slices.Contains(got, domain.EventJiraIssueUnreachable) {
		t.Fatalf("events = %v, want the status change diffed in the same cycle and no unreachable", got)
	}
	meta := decodeMeta[events.JiraIssueKeyChangedMetadata](t, evts[0])
	if meta.IssueKey != "OPS-7" || meta.IssueID != "10001" || meta.Project != "OPS" || meta.OldIssueKey != "ENG-1" || meta.OldProject != "ENG" {
		t.Errorf("key_changed metadata = %+v", meta)
	}
	status := decodeMeta[events.JiraIssueStatusChangedMetadata](t, *findEvent(evts, domain.EventJiraIssueStatusChanged))
	if status.IssueKey != "OPS-7" || status.Project != "OPS" || status.IssueID != "10001" {
		t.Errorf("status_changed metadata = %+v, want it under the new key", status)
	}
	searches, _ := fx.jira.reads()
	if !slices.Contains(searches, "id IN (10001)") {
		t.Errorf("searches = %v, want the refresh by id", searches)
	}

	after := fx.entity(t, jiraSite, "OPS-7")
	if after == nil || after.ID != before.ID || after.URL != jiraSite+"/browse/OPS-7" || after.State != "active" {
		t.Errorf("entity after the move = %+v, want %s renamed in place", after, before.ID)
	}
	if old := fx.entity(t, jiraSite, "ENG-1"); old != nil {
		t.Errorf("the old key still resolves: %+v", old)
	}
	if n := fx.countEntities(t); n != 1 {
		t.Errorf("%d jira entities, want 1", n)
	}
	if task, err := fx.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, taskID); err != nil || task == nil || task.EntityID != before.ID || task.Status != "queued" {
		t.Errorf("task = %+v err=%v, want it still on the entity and open", task, err)
	}
	if a, err := fx.stores.Artifacts.Get(context.Background(), runmode.LocalDefaultOrgID, art.ID); err != nil || a.Target != "OPS-7" || a.DedupKey != art.DedupKey {
		t.Errorf("artifact = %+v err=%v, want its target moved and its key kept", a, err)
	}

	if evts := fx.cycle(t, jiraSite, engAndOps()); len(evts) != 0 {
		t.Fatalf("next cycle emitted %v, want the move not re-emitted", eventTypes(evts))
	}
}

// TestRefreshJira_MoveIntoAnUnconfiguredProject: the entity is renamed, the
// move is key_changed, and the entity retires as unreachable with reason
// moved, naming the project the issue left so that project's teams hear it.
func TestRefreshJira_MoveIntoAnUnconfiguredProject(t *testing.T) {
	is := &fakeJiraIssue{ID: "10002", Keys: []string{"ENG-2"}, ProjectID: "100", Status: "In Progress", Assignee: "Alice", Summary: "Leaving", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, is)
	fx.cycle(t, jiraSite, engOnly())
	before := fx.entity(t, jiraSite, "ENG-2")

	fx.jira.update(func() {
		is.Keys, is.ProjectID, is.Updated = append(is.Keys, "SEC-3"), "300", "2026-10-01T11:00:00.000+0000"
	})
	evts := fx.cycle(t, jiraSite, engOnly())
	if got := eventTypes(evts); !slices.Equal(got, []string{domain.EventJiraIssueKeyChanged, domain.EventJiraIssueUnreachable}) {
		t.Fatalf("events = %v, want key_changed then unreachable", got)
	}
	gone := decodeMeta[events.JiraIssueUnreachableMetadata](t, evts[1])
	if gone.Reason != events.JiraUnreachableMoved || gone.Project != "ENG" || gone.IssueKey != "SEC-3" || gone.IssueID != "10002" {
		t.Errorf("unreachable metadata = %+v, want moved, naming ENG", gone)
	}
	if e := fx.entity(t, jiraSite, "SEC-3"); e == nil || e.ID != before.ID {
		t.Errorf("SEC-3 = %+v, want the entity renamed before it retires", e)
	}
}

// TestRefreshJira_ProjectKeyRename: a project whose key is renamed keeps its
// id, so every entity in it is renamed and kept — even when the rules still
// name the old key — rather than read as a move.
func TestRefreshJira_ProjectKeyRename(t *testing.T) {
	a := &fakeJiraIssue{ID: "10011", Keys: []string{"ENG-11"}, ProjectID: "100", Status: "In Progress", Assignee: "Alice", Summary: "a", Updated: "2026-10-01T10:00:00.000+0000"}
	b := &fakeJiraIssue{ID: "10012", Keys: []string{"ENG-12"}, ProjectID: "100", Status: "To Do", Summary: "b", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, a, b)
	fx.cycle(t, jiraSite, engOnly())

	fx.jira.update(func() {
		a.Keys, a.Updated = append(a.Keys, "CORE-11"), "2026-10-01T11:00:00.000+0000"
		b.Keys, b.Updated = append(b.Keys, "CORE-12"), "2026-10-01T11:00:00.000+0000"
	})
	evts := fx.cycle(t, jiraSite, engOnly())
	if got := findEvents(evts, domain.EventJiraIssueKeyChanged); len(got) != 2 {
		t.Fatalf("events = %v, want key_changed for both issues", eventTypes(evts))
	}
	if slices.Contains(eventTypes(evts), domain.EventJiraIssueUnreachable) {
		t.Fatalf("events = %v: a project key rename retired an entity", eventTypes(evts))
	}
	for _, key := range []string{"CORE-11", "CORE-12"} {
		if e := fx.entity(t, jiraSite, key); e == nil || e.State != "active" {
			t.Errorf("%s = %+v, want an active renamed entity", key, e)
		}
	}
	if n := fx.countEntities(t); n != 2 {
		t.Errorf("%d jira entities, want 2", n)
	}
}

// TestRefreshJira_MovedThenReopenedIssueReactivatesItsEntity: an issue that
// finished, moved while closed, and came back reopens its original entity
// rather than a new one, and the reopening still reports the move.
func TestRefreshJira_MovedThenReopenedIssueReactivatesItsEntity(t *testing.T) {
	is := &fakeJiraIssue{ID: "10021", Keys: []string{"ENG-21"}, ProjectID: "100", Status: "In Progress", Assignee: "Alice", Summary: "Again", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, is)
	fx.cycle(t, jiraSite, engAndOps())
	original := fx.entity(t, jiraSite, "ENG-21")

	// Done: the diff emits completed, and the router's close is what retires
	// the entity — stood in for here.
	fx.jira.update(func() { is.Status, is.Updated = "Done", "2026-10-01T11:00:00.000+0000" })
	fx.cycle(t, jiraSite, engAndOps())
	if _, err := fx.stores.Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, original.ID); err != nil {
		t.Fatal(err)
	}

	fx.jira.update(func() {
		is.Keys, is.ProjectID, is.Status, is.Updated = append(is.Keys, "OPS-4"), "200", "In Progress", "2026-10-02T10:00:00.000+0000"
	})
	evts := fx.cycle(t, jiraSite, engAndOps())
	e := fx.entity(t, jiraSite, "OPS-4")
	if e == nil || e.ID != original.ID || e.State != "active" {
		t.Fatalf("OPS-4 = %+v, want the original entity %s reactivated", e, original.ID)
	}
	if n := fx.countEntities(t); n != 1 {
		t.Errorf("%d jira entities, want 1", n)
	}
	kc := findEvent(evts, domain.EventJiraIssueKeyChanged)
	if kc == nil {
		t.Fatalf("events = %v, want the reopening to report the move", eventTypes(evts))
	}
	if meta := decodeMeta[events.JiraIssueKeyChangedMetadata](t, *kc); meta.OldIssueKey != "ENG-21" || meta.IssueKey != "OPS-4" {
		t.Errorf("key_changed metadata = %+v", meta)
	}
}

// TestRefreshJira_SiteSwitch: rows from the previous site retire with reason
// scope_changed and no request about them, and an issue on the new site that
// shares an old row's key and id gets an entity of its own.
func TestRefreshJira_SiteSwitch(t *testing.T) {
	is := &fakeJiraIssue{ID: "10031", Keys: []string{"ENG-31"}, ProjectID: "100", Status: "In Progress", Assignee: "Alice", Summary: "New site", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, is)
	const oldSite = "https://old.example.com"
	old, _, err := fx.stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "jira", oldSite, "ENG-31", "10031", "issue", "Old site", oldSite+"/browse/ENG-31")
	if err != nil {
		t.Fatal(err)
	}

	evts := fx.cycle(t, jiraSite, engOnly())
	retired := findEvents(evts, domain.EventJiraIssueUnreachable)
	if len(retired) != 1 || retired[0].EntityID == nil || *retired[0].EntityID != old.ID {
		t.Fatalf("events = %v, want one unreachable for the old site's row", eventTypes(evts))
	}
	if meta := decodeMeta[events.JiraIssueUnreachableMetadata](t, retired[0]); meta.Reason != events.JiraUnreachableScopeChanged {
		t.Errorf("reason = %q, want scope_changed", meta.Reason)
	}
	_, gets := fx.jira.reads()
	if len(gets) != 0 {
		t.Errorf("GETs = %v, want no request about the old site's issue", gets)
	}
	fresh := fx.entity(t, jiraSite, "ENG-31")
	if fresh == nil || fresh.ID == old.ID || fresh.ExternalID != "10031" {
		t.Fatalf("new site's ENG-31 = %+v, want its own entity", fresh)
	}
	if assigned := findEvent(evts, domain.EventJiraIssueAssigned); assigned == nil || *assigned.EntityID != fresh.ID {
		t.Errorf("events = %v, want the new site's issue discovered on its own entity", eventTypes(evts))
	}
	if o, _ := fx.stores.Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, old.ID); o == nil || o.Title != "Old site" {
		t.Errorf("old site's row = %+v, want it untouched", o)
	}
}

// TestRefreshJira_ProjectRecreatedUnderTheSameKey: the old project's issues are
// gone and new ones carry the same keys. While an old row is still active the
// new issue is skipped; once it retires the new issue gets an entity of its
// own, and the old one's history is not handed to it.
func TestRefreshJira_ProjectRecreatedUnderTheSameKey(t *testing.T) {
	gone := &fakeJiraIssue{ID: "10041", Keys: []string{"ENG-1"}, ProjectID: "100", Status: "In Progress", Assignee: "Alice", Summary: "Old", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, gone)
	fx.cycle(t, jiraSite, engOnly())
	old := fx.entity(t, jiraSite, "ENG-1")

	reborn := &fakeJiraIssue{ID: "20041", Keys: []string{"ENG-1"}, ProjectID: "900", Status: "In Progress", Assignee: "Alice", Summary: "New", Updated: "2026-10-02T10:00:00.000+0000"}
	fx.jira.update(func() {
		gone.Gone = true
		fx.jira.issues = append(fx.jira.issues, reborn)
	})
	if evts := fx.cycle(t, jiraSite, engOnly()); slices.Contains(eventTypes(evts), domain.EventJiraIssueAssigned) {
		t.Fatalf("events = %v: the new issue took a key an active row still holds", eventTypes(evts))
	}
	if e := fx.entity(t, jiraSite, "ENG-1"); e == nil || e.ID != old.ID || e.ExternalID != "10041" {
		t.Fatalf("ENG-1 = %+v, want the old row untouched while it is active", e)
	}

	// The old issue is confirmed gone; the router's close is stood in for.
	if _, err := fx.db.Exec(`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().Add(-2*jiraUnreachableGrace), old.ID); err != nil {
		t.Fatal(err)
	}
	evts := fx.cycle(t, jiraSite, engOnly())
	unreachable := findEvent(evts, domain.EventJiraIssueUnreachable)
	if unreachable == nil || decodeMeta[events.JiraIssueUnreachableMetadata](t, *unreachable).Reason != events.JiraUnreachableNotFound {
		t.Fatalf("events = %v, want the old issue confirmed not_found", eventTypes(evts))
	}
	if _, err := fx.stores.Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, old.ID); err != nil {
		t.Fatal(err)
	}

	evts = fx.cycle(t, jiraSite, engOnly())
	fresh := fx.entity(t, jiraSite, "ENG-1")
	if fresh == nil || fresh.ID == old.ID || fresh.ExternalID != "20041" {
		t.Fatalf("ENG-1 = %+v, want a new entity for the new issue", fresh)
	}
	if assigned := findEvent(evts, domain.EventJiraIssueAssigned); assigned == nil || *assigned.EntityID != fresh.ID {
		t.Errorf("events = %v, want the new issue's assignment on its own entity", eventTypes(evts))
	}
	if o, _ := fx.stores.Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, old.ID); o == nil || o.State != "closed" || o.Title != "Old" {
		t.Errorf("old row = %+v, want it closed with its history", o)
	}
}

// TestRefreshJira_UpgradedRowsLearnTheirIDs: rows written before issue ids
// were recorded are refreshed by key once, learn the id from the answer, and
// are refreshed by id from then on.
func TestRefreshJira_UpgradedRowsLearnTheirIDs(t *testing.T) {
	// Unassigned, under rules with no pickup set: no discovery query reaches
	// it, so only the refresh does.
	is := &fakeJiraIssue{ID: "10051", Keys: []string{"ENG-51"}, ProjectID: "100", Status: "In Progress", Summary: "Legacy", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, is)
	legacy, _, err := fx.stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "jira", jiraSite, "ENG-51", "", "issue", "Legacy", jiraSite+"/browse/ENG-51")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := json.Marshal(domain.JiraSnapshot{Key: "ENG-51", Summary: "Legacy", Status: "In Progress", StatusID: "st-In Progress", UpdatedAt: "2026-10-01T09:00:00.000+0000"})
	if ok, err := fx.stores.Entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, legacy.ID, string(snap), legacy.PollSeq); err != nil || !ok {
		t.Fatalf("seed legacy snapshot: ok=%v err=%v", ok, err)
	}
	rules := JiraRules{{Key: "ENG", DoneMembers: jiraRefs("Done")}}

	if evts := fx.cycle(t, jiraSite, rules); len(evts) != 0 {
		t.Fatalf("events = %v: a legacy snapshot was diffed as a first discovery", eventTypes(evts))
	}
	searches, _ := fx.jira.reads()
	if !slices.Contains(searches, "key IN (ENG-51)") {
		t.Errorf("searches = %v, want the legacy row read by key", searches)
	}
	if e := fx.entity(t, jiraSite, "ENG-51"); e == nil || e.ID != legacy.ID || e.ExternalID != "10051" {
		t.Fatalf("entity = %+v, want the id learned", e)
	}

	fx.cycle(t, jiraSite, rules)
	searches, _ = fx.jira.reads()
	if !slices.Contains(searches, "id IN (10051)") || slices.Contains(searches, "key IN (ENG-51)") {
		t.Errorf("searches = %v, want the row read by id once it carries one", searches)
	}
}

// TestRefreshJira_DiscoveryLearnsAClosedRowsID: discovery reaching an issue
// whose closed entity predates ids reopens that entity and stamps the id on it
// rather than creating a second one.
func TestRefreshJira_DiscoveryLearnsAClosedRowsID(t *testing.T) {
	is := &fakeJiraIssue{ID: "10061", Keys: []string{"ENG-61"}, ProjectID: "100", Status: "To Do", Summary: "Back", Updated: "2026-10-01T10:00:00.000+0000"}
	fx := newJiraIdentityFixture(t, is)
	closed, _, err := fx.stores.Entities.FindOrCreateSystem(context.Background(), runmode.LocalDefaultOrgID, "jira", jiraSite, "ENG-61", "", "issue", "Back", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.stores.Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, closed.ID); err != nil {
		t.Fatal(err)
	}
	fx.cycle(t, jiraSite, engOnly())
	e := fx.entity(t, jiraSite, "ENG-61")
	if e == nil || e.ID != closed.ID || e.ExternalID != "10061" || e.State != "active" {
		t.Fatalf("ENG-61 = %+v, want the closed row reopened with its id", e)
	}
	if n := fx.countEntities(t); n != 1 {
		t.Errorf("%d jira entities, want 1", n)
	}
}

// TestDiffJiraSnapshots_KeyChangedFirst pins the ordering and the first
// discovery rule: key_changed precedes everything a diff finds, a legacy
// snapshot (key, no id) is not a first discovery, and a zero one is.
func TestDiffJiraSnapshots_KeyChangedFirst(t *testing.T) {
	prev := domain.JiraSnapshot{Key: "ENG-1", Status: "To Do", StatusID: "1"}
	curr := domain.JiraSnapshot{ID: "10001", Key: "OPS-1", Status: "Done", StatusID: "3", Assignee: "Alice"}
	evts := DiffJiraSnapshots(prev, curr, testEntityID, jiraRefs("Done"))
	got := eventTypes(evts)
	if len(got) == 0 || got[0] != domain.EventJiraIssueKeyChanged {
		t.Fatalf("events = %v, want key_changed first", got)
	}
	for _, evt := range evts {
		var m struct {
			IssueID  string `json:"issue_id"`
			IssueKey string `json:"issue_key"`
		}
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
			t.Fatal(err)
		}
		if m.IssueID != "10001" || m.IssueKey != "OPS-1" {
			t.Errorf("%s metadata = %+v, want the current key and the issue id", evt.EventType, m)
		}
	}
	if evts := DiffJiraSnapshots(domain.JiraSnapshot{}, curr, testEntityID, nil); len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueAssigned {
		t.Errorf("zero prev = %v, want first discovery", eventTypes(evts))
	}
	if evts := DiffJiraSnapshots(domain.JiraSnapshot{Key: "OPS-1", Assignee: "Alice"}, curr, testEntityID, nil); slices.Contains(eventTypes(evts), domain.EventJiraIssueAssigned) {
		t.Errorf("legacy prev = %v, want it diffed rather than treated as first discovery", eventTypes(evts))
	}
}
