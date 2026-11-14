package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// identityFixture serves the three REST reads the identity helper makes, from
// a path → body table, answering 404 for any path it does not hold, and counts
// the requests it was sent.
type identityFixture struct {
	mu     sync.Mutex
	bodies map[string]string
	status map[string]int
	hits   map[string]int
}

func (f *identityFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	f.hits[r.URL.Path]++
	if code, ok := f.status[r.URL.Path]; ok {
		w.WriteHeader(code)
		return
	}
	body, ok := f.bodies[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return
	}
	_, _ = w.Write([]byte(body))
}

func (f *identityFixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *identityFixture) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.hits {
		n += c
	}
	return n
}

func newIdentityFixture(t *testing.T, bodies map[string]string) (*identityFixture, *Client) {
	t.Helper()
	f := &identityFixture{bodies: bodies, status: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, testClient(srv.URL)
}

const (
	pullBody  = `{"number": 7, "node_id": "PR_kw7", "state": "open", "base": {"repo": {"id": 4242, "full_name": "octo/api"}}}`
	issueBody = `{"number": 9, "node_id": "I_kw9", "state": "closed"}`
	repoBody  = `{"id": 4242, "full_name": "octo/api"}`
)

func TestIdentityReader_PullRequest(t *testing.T) {
	f, c := newIdentityFixture(t, map[string]string{"/repos/octo/api/pulls/7": pullBody})
	r := NewIdentityReader()
	got, err := r.Object(context.Background(), c, "octo", "api", 7)
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	want := domain.GitHubIdentity{RepositoryID: "4242", Repository: "octo/api", NodeID: "PR_kw7", Kind: domain.EntityKindPullRequest}
	if got != want {
		t.Errorf("Object = %+v, want %+v", got, want)
	}
	// The pull request's base repository seeds the cache, and a second read
	// of the pull request is answered from it.
	if _, err := r.Repository(context.Background(), c, "Octo", "API"); err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if again, err := r.Object(context.Background(), c, "octo", "api", 7); err != nil || again != want {
		t.Fatalf("second Object = %+v err=%v", again, err)
	}
	if n := f.total(); n != 1 {
		t.Errorf("requests = %d, want 1 (the pull request read answers its repository)", n)
	}
}

func TestIdentityReader_Issue(t *testing.T) {
	f, c := newIdentityFixture(t, map[string]string{
		"/repos/octo/old-api/issues/9": issueBody,
		"/repos/octo/old-api":          repoBody,
	})
	got, err := NewIdentityReader().Object(context.Background(), c, "octo", "old-api", 9)
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	// The old name answers the repository's id and the name GitHub gives it now.
	want := domain.GitHubIdentity{RepositoryID: "4242", Repository: "octo/api", NodeID: "I_kw9", Kind: domain.EntityKindIssue}
	if got != want {
		t.Errorf("Object = %+v, want %+v", got, want)
	}
	if f.count("/repos/octo/old-api/pulls/9") != 1 {
		t.Errorf("the pull request was not asked for first")
	}
}

func TestIdentityReader_RepositoryIsCachedPerReader(t *testing.T) {
	f, c := newIdentityFixture(t, map[string]string{"/repos/octo/api": repoBody})
	r := NewIdentityReader()
	for range 2 {
		got, err := r.Repository(context.Background(), c, "octo", "api")
		if err != nil {
			t.Fatalf("Repository: %v", err)
		}
		if got != (domain.GitHubIdentity{RepositoryID: "4242", Repository: "octo/api"}) {
			t.Errorf("Repository = %+v", got)
		}
	}
	if n := f.count("/repos/octo/api"); n != 1 {
		t.Errorf("repository reads = %d, want 1", n)
	}
	// A nil reader reads every time.
	var none *IdentityReader
	if _, err := none.Repository(context.Background(), c, "octo", "api"); err != nil {
		t.Fatalf("nil reader: %v", err)
	}
	if n := f.count("/repos/octo/api"); n != 2 {
		t.Errorf("repository reads = %d, want 2 after an uncached read", n)
	}
}

func TestIdentityReader_MismatchedAnswerIsAnError(t *testing.T) {
	cases := map[string]map[string]string{
		"pull request with another number": {"/repos/octo/api/pulls/7": strings.Replace(pullBody, `"number": 7`, `"number": 8`, 1)},
		"pull request with no node id":     {"/repos/octo/api/pulls/7": strings.Replace(pullBody, `"PR_kw7"`, `""`, 1)},
		"pull request with no repository":  {"/repos/octo/api/pulls/7": `{"number": 7, "node_id": "PR_kw7"}`},
		"issue that is a pull request": {
			"/repos/octo/api/issues/7": `{"number": 7, "node_id": "PR_kw7", "pull_request": {"url": "x"}}`,
			"/repos/octo/api":          repoBody,
		},
		"issue with another number": {
			"/repos/octo/api/issues/7": strings.Replace(issueBody, `"number": 9`, `"number": 8`, 1),
			"/repos/octo/api":          repoBody,
		},
	}
	for name, bodies := range cases {
		t.Run(name, func(t *testing.T) {
			_, c := newIdentityFixture(t, bodies)
			if got, err := NewIdentityReader().Object(context.Background(), c, "octo", "api", 7); err == nil {
				t.Errorf("Object = %+v, want an error", got)
			}
		})
	}
}

func TestIdentityReader_FailedRequestIsAnError(t *testing.T) {
	f, c := newIdentityFixture(t, map[string]string{})
	f.status["/repos/octo/api/pulls/7"] = http.StatusForbidden
	if _, err := NewIdentityReader().Object(context.Background(), c, "octo", "api", 7); err == nil {
		t.Fatal("a 403 on the pull request read answered an identity")
	}
	// Only a 404 asks for the issue: any other refusal is the answer.
	if f.count("/repos/octo/api/issues/7") != 0 {
		t.Errorf("a 403 fell through to the issue read")
	}
	// Neither object exists.
	if _, err := NewIdentityReader().Object(context.Background(), c, "octo", "api", 8); err == nil {
		t.Error("a number with neither a pull request nor an issue answered an identity")
	}
	r := NewIdentityReader()
	if _, err := r.Repository(context.Background(), c, "octo", "api"); err == nil {
		t.Fatal("a missing repository answered an identity")
	}
	// A failure is not cached.
	f.mu.Lock()
	f.bodies["/repos/octo/api"] = repoBody
	f.mu.Unlock()
	if _, err := r.Repository(context.Background(), c, "octo", "api"); err != nil {
		t.Errorf("Repository after the repository appeared: %v", err)
	}
}

// branchFixture answers the branch lookup's list of open pull requests from a
// head → body table, answering an empty list for a head it does not hold, and
// records each lookup's query.
type branchFixture struct {
	mu      sync.Mutex
	bodies  map[string]string
	status  int
	queries []url.Values
	other   int
}

func (f *branchFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/repos/octo/api/pulls" {
		f.other++
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.queries = append(f.queries, r.URL.Query())
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	body, ok := f.bodies[r.URL.Query().Get("head")]
	if !ok {
		body = `[]`
	}
	_, _ = w.Write([]byte(body))
}

func newBranchFixture(t *testing.T, bodies map[string]string) (*branchFixture, *Client) {
	t.Helper()
	f := &branchFixture{bodies: bodies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, testClient(srv.URL)
}

func branchPull(number int, nodeID, ref string) string {
	return fmt.Sprintf(`{"number": %d, "node_id": %q, "head": {"ref": %q}, "base": {"repo": {"id": 4242, "full_name": "octo/api"}}}`, number, nodeID, ref)
}

// The lookup asks for the open pull requests from the repository's own branch,
// answers the one it finds with what GitHub calls it, and leaves that pull
// request and its repository in the cache for the reads after it.
func TestIdentityReader_OpenPullForBranch(t *testing.T) {
	const branch = "feat/a&b#c+d"
	f, c := newBranchFixture(t, map[string]string{"octo:" + branch: "[" + branchPull(7, "PR_kw7", branch) + "]"})
	r := NewIdentityReader()
	got, err := r.OpenPullForBranch(context.Background(), c, "octo", "api", branch)
	if err != nil {
		t.Fatalf("OpenPullForBranch: %v", err)
	}
	want := BranchPull{Number: 7, Identity: domain.GitHubIdentity{RepositoryID: "4242", Repository: "octo/api", NodeID: "PR_kw7", Kind: domain.EntityKindPullRequest}}
	if got != want {
		t.Errorf("OpenPullForBranch = %+v, want %+v", got, want)
	}
	f.mu.Lock()
	queries := f.queries
	f.mu.Unlock()
	wantQuery := url.Values{"head": {"octo:" + branch}, "state": {"open"}, "per_page": {"2"}}
	if len(queries) != 1 || queries[0].Encode() != wantQuery.Encode() {
		t.Errorf("lookups = %v, want one with %v", queries, wantQuery)
	}

	if again, err := r.Object(context.Background(), c, "Octo", "API", 7); err != nil || again != want.Identity {
		t.Errorf("Object after the lookup = %+v err=%v, want %+v from the cache", again, err, want.Identity)
	}
	if _, err := r.Repository(context.Background(), c, "octo", "api"); err != nil {
		t.Errorf("Repository after the lookup: %v", err)
	}
	// The answer about the branch itself is not cached: it changes.
	if _, err := r.OpenPullForBranch(context.Background(), c, "octo", "api", branch); err != nil {
		t.Fatalf("second OpenPullForBranch: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 2 || f.other != 0 {
		t.Errorf("requests = %d lookups and %d others, want 2 lookups and nothing else", len(f.queries), f.other)
	}
}

// None and several are the same answer: no pull request is named.
func TestIdentityReader_OpenPullForBranchNamesOneOrNone(t *testing.T) {
	_, c := newBranchFixture(t, map[string]string{
		"octo:two": "[" + branchPull(7, "PR_kw7", "two") + "," + branchPull(8, "PR_kw8", "two") + "]",
	})
	for _, branch := range []string{"none", "two"} {
		got, err := NewIdentityReader().OpenPullForBranch(context.Background(), c, "octo", "api", branch)
		if err != nil || got != (BranchPull{}) {
			t.Errorf("OpenPullForBranch(%q) = %+v err=%v, want no pull request and no error", branch, got, err)
		}
	}
}

func TestIdentityReader_OpenPullForBranchMismatchIsAnError(t *testing.T) {
	cases := map[string]string{
		"a pull request from another branch": "[" + branchPull(7, "PR_kw7", "other") + "]",
		"several, one from another branch":   "[" + branchPull(7, "PR_kw7", "feature") + "," + branchPull(8, "PR_kw8", "Feature") + "]",
		"a pull request with no node id":     "[" + branchPull(7, "", "feature") + "]",
		"a pull request with no number":      "[" + branchPull(0, "PR_kw7", "feature") + "]",
		"a pull request with no repository":  `[{"number": 7, "node_id": "PR_kw7", "head": {"ref": "feature"}}]`,
		"an object rather than a list":       branchPull(7, "PR_kw7", "feature"),
		"a head that is not an object":       `[{"number": 7, "head": "feature"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, c := newBranchFixture(t, map[string]string{"octo:feature": body})
			if got, err := NewIdentityReader().OpenPullForBranch(context.Background(), c, "octo", "api", "feature"); err == nil {
				t.Errorf("OpenPullForBranch = %+v, want an error", got)
			}
		})
	}
	f, c := newBranchFixture(t, nil)
	f.status = http.StatusNotFound
	if got, err := NewIdentityReader().OpenPullForBranch(context.Background(), c, "octo", "api", "feature"); err == nil {
		t.Errorf("OpenPullForBranch on a refused lookup = %+v, want an error", got)
	}
}

func TestTokenGetter(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v3/repos/octo/api" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(repoBody))
	}))
	defer srv.Close()
	g := NewTokenGetter(srv.URL+"/api/v3/", func(context.Context) (string, error) { return "real", nil }, nil)
	got, err := NewIdentityReader().Repository(context.Background(), g, "octo", "api")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if got.RepositoryID != "4242" || sawAuth != "token real" {
		t.Errorf("Repository = %+v with Authorization %q", got, sawAuth)
	}
	if _, err := g.Get(context.Background(), "/repos/octo/missing"); !IsHTTP404(err) {
		t.Errorf("Get of a missing path = %v, want a 404 HTTPError", err)
	}
}
