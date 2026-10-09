package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const (
	// ghOldHost is the host the org polled first; ghNewHost the one its GitHub
	// base URL names afterwards.
	ghOldHost = "https://github.com"
	ghNewHost = "https://ghe.example.com"
)

// fakeGitHubHost is one GitHub deployment. It lists each repo's open pull
// requests with a conditional request keyed on an ETag that changes only when
// the listing does, answers a single-PR read for the numbers in basics, answers
// every GraphQL node with null (so the refresh writes nothing), and records
// every request so a test can tell what was asked of this host.
type fakeGitHubHost struct {
	t *testing.T

	mu       sync.Mutex
	listings map[string][]map[string]any // "owner/repo" → open PRs
	basics   map[string]map[string]any   // "owner/repo#N" → single-PR body
	nodeIDs  []string                    // every node id a GraphQL refresh asked for
	paths    []string                    // every REST path requested
	notMod   int                         // listings answered 304
}

func newFakeGitHubHost(t *testing.T) (*fakeGitHubHost, *ghclient.Client) {
	t.Helper()
	h := &fakeGitHubHost{t: t, listings: map[string][]map[string]any{}, basics: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	return h, ghclient.NewClient(srv.URL, "tok")
}

// listPR adds an open pull request to repo's listing.
func (h *fakeGitHubHost) listPR(repo string, number int, nodeID, title string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listings[repo] = append(h.listings[repo], map[string]any{
		"number": number, "node_id": nodeID, "title": title, "state": "open", "draft": false,
		"html_url":   "https://example.invalid/" + repo + "/pull/" + fmt.Sprint(number),
		"created_at": "2026-09-01T10:00:00Z", "updated_at": "2026-09-02T10:00:00Z",
		"user":                map[string]any{"login": "alice"},
		"head":                map[string]any{"sha": "sha-" + nodeID, "ref": "feature", "repo": map[string]any{"full_name": repo}},
		"base":                map[string]any{"ref": "main", "repo": map[string]any{"full_name": repo}},
		"requested_reviewers": []any{}, "requested_teams": []any{}, "labels": []any{},
	})
}

// basicPR makes the single-PR read for sourceID answer an open pull request
// carrying nodeID.
func (h *fakeGitHubHost) basicPR(sourceID, nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _, number := domain.SplitGitHubEntitySourceID(sourceID)
	h.basics[sourceID] = map[string]any{
		"number": number, "node_id": nodeID, "title": "Stub", "state": "open", "merged": false,
		"html_url": "https://example.invalid/" + sourceID,
		"user":     map[string]any{"login": "bob"},
		"head":     map[string]any{"sha": "sha-" + nodeID, "ref": "feat"}, "base": map[string]any{"ref": "main"},
	}
}

func (h *fakeGitHubHost) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/graphql") {
		var body struct {
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.t.Errorf("decode graphql body: %v", err)
		}
		h.nodeIDs = append(h.nodeIDs, body.Variables.IDs...)
		nodes := make([]any, len(body.Variables.IDs))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	h.paths = append(h.paths, path)
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok {
		h.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
		return
	}
	if repo, ok := strings.CutSuffix(rest, "/pulls"); ok {
		listing, _ := json.Marshal(h.listings[repo])
		etag := fmt.Sprintf(`"%x"`, listing)
		if r.Header.Get("If-None-Match") == etag {
			h.notMod++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(listing)
		return
	}
	if repo, number, ok := strings.Cut(rest, "/pulls/"); ok {
		if body, found := h.basics[repo+"#"+number]; found {
			_ = json.NewEncoder(w).Encode(body)
			return
		}
	}
	h.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	http.Error(w, "unexpected", http.StatusNotFound)
}

func (h *fakeGitHubHost) askedNodes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.nodeIDs)
}

func (h *fakeGitHubHost) askedPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.paths)
}

func (h *fakeGitHubHost) notModified() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.notMod
}

// githubScopeFixture is one org's tracker over a real SQLite store, polling
// whichever fake host a cycle names.
type githubScopeFixture struct {
	stores db.Stores
	pub    *recordingPublisher
	tr     *Tracker
}

func newGitHubScopeFixture(t *testing.T) *githubScopeFixture {
	t.Helper()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, runmode.LocalDefaultOrgID)
	return &githubScopeFixture{stores: stores, pub: pub, tr: tr}
}

// cycle runs one GitHub poll cycle on host the way the poller does — the
// out-of-host retirement first, then the refresh — and returns what it
// published.
func (fx *githubScopeFixture) cycle(t *testing.T, host string, client *ghclient.Client, repos []string) []domain.Event {
	t.Helper()
	fx.pub.mu.Lock()
	fx.pub.events = nil
	fx.pub.mu.Unlock()
	fx.tr.RetireGitHubOutOfScope(context.Background(), host)
	if _, _, err := fx.tr.RefreshGitHub(context.Background(), host, client, "", repos, nil); err != nil {
		t.Fatalf("RefreshGitHub on %s: %v", host, err)
	}
	return fx.pub.nonSystemEvents()
}

// seedPR creates an active pull request entity on host, with snap stored when
// it is non-nil and as a snapshot-less stub otherwise.
func (fx *githubScopeFixture) seedPR(t *testing.T, host, sourceID string, snap *domain.PRSnapshot) *domain.Entity {
	t.Helper()
	ctx := context.Background()
	e, _, err := fx.stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "github", host, sourceID, "", "pr", "seeded "+sourceID, "")
	if err != nil {
		t.Fatalf("seed %s on %s: %v", sourceID, host, err)
	}
	if snap == nil {
		return e
	}
	raw, _ := json.Marshal(snap)
	if ok, err := fx.stores.Entities.UpdateSnapshotCASSystem(ctx, runmode.LocalDefaultOrgID, e.ID, string(raw), e.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot for %s: ok=%v err=%v", sourceID, ok, err)
	}
	got, err := fx.stores.Entities.GetSystem(ctx, runmode.LocalDefaultOrgID, e.ID)
	if err != nil || got == nil {
		t.Fatalf("reread %s: %v", sourceID, err)
	}
	return got
}

// closeEntity stands in for the router's close, which a terminating event
// drives in production.
func (fx *githubScopeFixture) closeEntity(t *testing.T, id string) {
	t.Helper()
	if _, err := fx.stores.Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, id); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}

func (fx *githubScopeFixture) entity(t *testing.T, host, sourceID string) *domain.Entity {
	t.Helper()
	e, err := fx.stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", host, sourceID)
	if err != nil {
		t.Fatalf("GetBySource %s on %s: %v", sourceID, host, err)
	}
	return e
}

func unreachableByEntity(t *testing.T, evts []domain.Event) map[string]events.GitHubPRUnreachableMetadata {
	t.Helper()
	out := map[string]events.GitHubPRUnreachableMetadata{}
	for _, evt := range findEvents(evts, domain.EventGitHubPRUnreachable) {
		if evt.EntityID == nil {
			t.Fatalf("unreachable event without an entity: %+v", evt)
		}
		if _, dup := out[*evt.EntityID]; dup {
			t.Errorf("entity %s retired twice in one cycle", *evt.EntityID)
		}
		out[*evt.EntityID] = decodeMetadata[events.GitHubPRUnreachableMetadata](t, evt)
	}
	return out
}

// TestRetireGitHubOutOfScope_EmitsUnreachableWithTheLastKnownFields: once the
// org's GitHub host changes, every active pull request polled from another host
// emits github:pr:unreachable exactly once per pass, carrying the host it was
// polled from, reason scope_changed and the fields of its stored snapshot — or,
// for a stub with no snapshot, the repo and number in its key. A pull request on
// the current host emits nothing. The method takes no client, so retiring
// cannot ask GitHub anything; the end-to-end test below pins that against a
// cycle that does hold one.
func TestRetireGitHubOutOfScope_EmitsUnreachableWithTheLastKnownFields(t *testing.T) {
	fx := newGitHubScopeFixture(t)
	snap := &domain.PRSnapshot{
		NodeID: "PR_old42", Number: 42, Repo: "octo/repo", Title: "Add widget", Author: "alice",
		State: "OPEN", IsDraft: true, HeadSHA: "abc123", Labels: []string{"bug", "ui"},
	}
	withSnap := fx.seedPR(t, ghOldHost, "octo/repo#42", snap)
	stub := fx.seedPR(t, ghOldHost, "octo/tools#7", nil)
	current := fx.seedPR(t, ghNewHost, "octo/repo#42", &domain.PRSnapshot{NodeID: "PR_new42", Number: 42, Repo: "octo/repo", State: "OPEN"})

	if n := fx.tr.RetireGitHubOutOfScope(context.Background(), ghNewHost); n != 2 {
		t.Errorf("retired = %d, want 2", n)
	}
	got := unreachableByEntity(t, fx.pub.nonSystemEvents())
	if len(got) != 2 {
		t.Fatalf("unreachable for %d entities, want the two on %s", len(got), ghOldHost)
	}
	if _, ok := got[current.ID]; ok {
		t.Error("the current host's pull request was retired")
	}

	want := events.GitHubPRUnreachableMetadata{
		Author: "alice", Repo: "octo/repo", PRNumber: 42, IsDraft: true, HeadSHA: "abc123",
		Labels: []string{"bug", "ui"}, Title: "Add widget",
		Host: ghOldHost, Reason: events.GitHubUnreachableScopeChanged,
	}
	if m := got[withSnap.ID]; !metadataEqual(m, want) {
		t.Errorf("metadata from the snapshot = %+v, want %+v", m, want)
	}
	wantStub := events.GitHubPRUnreachableMetadata{Repo: "octo/tools", PRNumber: 7, Host: ghOldHost, Reason: events.GitHubUnreachableScopeChanged}
	if m := got[stub.ID]; !metadataEqual(m, wantStub) {
		t.Errorf("metadata for the stub = %+v, want %+v (repo and number from its key)", m, wantStub)
	}

	// The rows are not moved or rewritten; closing them is the router's job.
	for _, e := range []*domain.Entity{withSnap, stub} {
		after, err := fx.stores.Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, e.ID)
		if err != nil || after == nil {
			t.Fatalf("reread %s: %v", e.SourceID, err)
		}
		if after.Scope != ghOldHost || after.State != "active" || after.SnapshotJSON != e.SnapshotJSON || after.PollSeq != e.PollSeq {
			t.Errorf("%s after retirement = scope %q state %q poll_seq %d; want it where it was", e.SourceID, after.Scope, after.State, after.PollSeq)
		}
	}

	// Once the router has closed them there is nothing left to retire.
	fx.closeEntity(t, withSnap.ID)
	fx.closeEntity(t, stub.ID)
	fx.pub.mu.Lock()
	fx.pub.events = nil
	fx.pub.mu.Unlock()
	if n := fx.tr.RetireGitHubOutOfScope(context.Background(), ghNewHost); n != 0 {
		t.Errorf("retired = %d after the close landed, want 0", n)
	}
	if evts := fx.pub.nonSystemEvents(); len(evts) != 0 {
		t.Errorf("events = %v after the close landed, want none", eventTypes(evts))
	}

	// No host is no answer: nothing is retired against it.
	if n := fx.tr.RetireGitHubOutOfScope(context.Background(), ""); n != 0 {
		t.Errorf("retired = %d against an empty host, want 0", n)
	}
}

func metadataEqual(a, b events.GitHubPRUnreachableMetadata) bool {
	return a.Author == b.Author && a.Repo == b.Repo && a.PRNumber == b.PRNumber && a.IsDraft == b.IsDraft &&
		a.HeadSHA == b.HeadSHA && slices.Equal(a.Labels, b.Labels) && a.Title == b.Title &&
		a.Host == b.Host && a.Reason == b.Reason
}

// TestRefreshGitHub_HostSwitch: the cycle after the org's host changes retires
// the old host's rows without a request about them, and everything else it does
// — discovery, the Phase 2 refresh, stub resolution — touches only rows on the
// new host. The new host's pull request that shares an old row's key gets an
// entity of its own.
func TestRefreshGitHub_HostSwitch(t *testing.T) {
	fx := newGitHubScopeFixture(t)
	old := fx.seedPR(t, ghOldHost, "octo/repo#42", &domain.PRSnapshot{NodeID: "PR_old42", Number: 42, Repo: "octo/repo", Title: "Old host's #42", State: "OPEN"})
	oldStub := fx.seedPR(t, ghOldHost, "octo/repo#7", nil)
	// Rows already on the new host: one with a snapshot the listing does not
	// carry, which the Phase 2 refresh reaches, and a stub, which stub
	// resolution reaches.
	fx.seedPR(t, ghNewHost, "octo/repo#9", &domain.PRSnapshot{NodeID: "PR_new9", Number: 9, Repo: "octo/repo", State: "OPEN"})
	fx.seedPR(t, ghNewHost, "octo/repo#5", nil)

	host, client := newFakeGitHubHost(t)
	host.listPR("octo/repo", 42, "PR_new42", "New host's #42")
	host.basicPR("octo/repo#5", "PR_new5")
	trackReposOn(t, fx.stores, runmode.LocalDefaultOrgID, ghNewHost, []string{"octo/repo"})

	evts := fx.cycle(t, ghNewHost, client, []string{"octo/repo"})

	retired := unreachableByEntity(t, evts)
	if len(retired) != 2 {
		t.Fatalf("events = %v, want one unreachable for each of the old host's rows", eventTypes(evts))
	}
	for _, id := range []string{old.ID, oldStub.ID} {
		m, ok := retired[id]
		if !ok {
			t.Errorf("no unreachable for %s", id)
			continue
		}
		if m.Host != ghOldHost || m.Reason != events.GitHubUnreachableScopeChanged {
			t.Errorf("unreachable for %s = host %q reason %q, want %s and scope_changed", id, m.Host, m.Reason, ghOldHost)
		}
	}

	// Nothing about the old host's rows reached the new host: not the old
	// row's node id, and not the old stub's owner/repo#N.
	nodes := host.askedNodes()
	if slices.Contains(nodes, "PR_old42") {
		t.Errorf("GraphQL asked the new host for the old host's node: %v", nodes)
	}
	paths := host.askedPaths()
	if slices.Contains(paths, "/repos/octo/repo/pulls/7") {
		t.Errorf("the old host's stub was resolved against the new host: %v", paths)
	}
	// The new host's own rows were refreshed and resolved as usual.
	if !slices.Contains(nodes, "PR_new9") {
		t.Errorf("GraphQL nodes = %v, want the new host's octo/repo#9 refreshed", nodes)
	}
	if !slices.Contains(paths, "/repos/octo/repo/pulls/5") {
		t.Errorf("requests = %v, want the new host's stub octo/repo#5 resolved", paths)
	}

	fresh := fx.entity(t, ghNewHost, "octo/repo#42")
	if fresh == nil || fresh.ID == old.ID || !strings.Contains(fresh.SnapshotJSON, "PR_new42") {
		t.Fatalf("new host's octo/repo#42 = %+v, want its own entity seeded from the new host", fresh)
	}
	after := fx.entity(t, ghOldHost, "octo/repo#42")
	if after == nil || after.ID != old.ID || after.Title != old.Title || after.SnapshotJSON != old.SnapshotJSON {
		t.Errorf("old host's row = %+v, want it untouched", after)
	}
}

// TestRefreshGitHub_SwitchingBackTracksTheOldHostAgain: an org that moves to
// another host and back finds the pull requests it left on the first host
// tracked again. The rows retired while it was away are the first host's own,
// closed with their history, so discovery reopens them rather than colliding
// with them, and the second host's rows retire in turn without touching them.
//
// The first host's open-PR listing is unchanged throughout. A conditional
// request replayed with the ETag stored before the move would answer 304 and
// discover nothing, leaving the reopened PR closed until something in its
// repository changed; the retirement drops that cursor so the first listing
// after the return is unconditional.
func TestRefreshGitHub_SwitchingBackTracksTheOldHostAgain(t *testing.T) {
	fx := newGitHubScopeFixture(t)
	org := runmode.LocalDefaultOrgID
	first, firstClient := newFakeGitHubHost(t)
	first.listPR("octo/repo", 42, "PR_first42", "First host's #42")
	second, secondClient := newFakeGitHubHost(t)
	second.listPR("octo/repo", 42, "PR_second42", "Second host's #42")
	trackReposOn(t, fx.stores, org, ghOldHost, []string{"octo/repo"})
	trackReposOn(t, fx.stores, org, ghNewHost, []string{"octo/repo"})
	repos := []string{"octo/repo"}

	// On the first host.
	fx.cycle(t, ghOldHost, firstClient, repos)
	original := fx.entity(t, ghOldHost, "octo/repo#42")
	if original == nil || original.State != "active" {
		t.Fatalf("first host's octo/repo#42 = %+v, want it tracked", original)
	}

	// Moved to the second host: the first host's row retires and closes.
	evts := fx.cycle(t, ghNewHost, secondClient, repos)
	if m, ok := unreachableByEntity(t, evts)[original.ID]; !ok || m.Host != ghOldHost {
		t.Fatalf("events = %v, want the first host's row retired", eventTypes(evts))
	}
	fx.closeEntity(t, original.ID)
	away := fx.entity(t, ghNewHost, "octo/repo#42")
	if away == nil || away.ID == original.ID {
		t.Fatalf("second host's octo/repo#42 = %+v, want its own entity", away)
	}

	// Back on the first host: the second host's row retires, and the first
	// host's pull request is tracked again on the row it always had.
	evts = fx.cycle(t, ghOldHost, firstClient, repos)
	retired := unreachableByEntity(t, evts)
	if m, ok := retired[away.ID]; !ok || m.Host != ghNewHost || len(retired) != 1 {
		t.Fatalf("unreachable = %+v, want only the second host's row", retired)
	}
	back := fx.entity(t, ghOldHost, "octo/repo#42")
	if back == nil || back.ID != original.ID {
		t.Fatalf("first host's octo/repo#42 = %+v, want the original row (id %s)", back, original.ID)
	}
	if back.State != "active" {
		t.Errorf("first host's octo/repo#42 is %s after the return, want active: an unchanged listing must not hide it", back.State)
	}
	if n := first.notModified(); n != 0 {
		t.Errorf("the first host answered %d conditional listings with 304 after the return, want the listing unconditional", n)
	}
	if nodes := first.askedNodes(); slices.Contains(nodes, "PR_second42") {
		t.Errorf("GraphQL asked the first host for the second host's node: %v", nodes)
	}
	if left := fx.entity(t, ghNewHost, "octo/repo#42"); left == nil || left.ID != away.ID || !strings.Contains(left.SnapshotJSON, "PR_second42") {
		t.Errorf("second host's row = %+v, want it where it was", left)
	}

	// The cursor is dropped once, by the retirement: the cycle after the
	// return replays the fresh ETag and is answered 304 again.
	fx.cycle(t, ghOldHost, firstClient, repos)
	if n := first.notModified(); n != 1 {
		t.Errorf("304s on the cycle after the return = %d, want 1", n)
	}
}
