package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// TestIdentityReader_CommentSubject: a comment read by its own id answers the
// number of the object it is on, from the link of that object the comment
// carries, in either numbering and under a GHES prefix. Each comment is read
// once per reader.
func TestIdentityReader_CommentSubject(t *testing.T) {
	f, c := newIdentityFixture(t, map[string]string{
		"/repos/octo/api/pulls/comments/9001":  `{"id": 9001, "node_id": "PRRC_kw", "pull_request_url": "https://api.github.com/repos/octo/api/pulls/7"}`,
		"/repos/octo/api/issues/comments/9001": `{"id": 9001, "node_id": "IC_kw", "issue_url": "https://ghe.example.com/api/v3/repos/octo/renamed/issues/9"}`,
	})
	r := NewIdentityReader()
	// The second spelling is answered from the cache: names fold case.
	for _, owner := range []string{"octo", "OCTO"} {
		if n, err := r.CommentSubject(context.Background(), c, owner, "api", domain.GitHubReviewComment, 9001); err != nil || n != 7 {
			t.Errorf("review comment's subject = %d, %v; want 7", n, err)
		}
		if n, err := r.CommentSubject(context.Background(), c, owner, "api", domain.GitHubIssueComment, 9001); err != nil || n != 9 {
			t.Errorf("conversation comment's subject = %d, %v; want 9", n, err)
		}
	}
	if n := f.total(); n != 2 {
		t.Errorf("reads = %d, want one per comment", n)
	}
}

func TestIdentityReader_CommentSubjectMismatchIsAnError(t *testing.T) {
	const path = "/repos/octo/api/pulls/comments/9001"
	cases := map[string]string{
		"another comment":                    `{"id": 9002, "pull_request_url": "https://api.github.com/repos/octo/api/pulls/7"}`,
		"no id":                              `{"pull_request_url": "https://api.github.com/repos/octo/api/pulls/7"}`,
		"no link":                            `{"id": 9001}`,
		"the other numbering's link":         `{"id": 9001, "issue_url": "https://api.github.com/repos/octo/api/issues/7"}`,
		"a link to another kind of object":   `{"id": 9001, "pull_request_url": "https://api.github.com/repos/octo/api/commits/7"}`,
		"a link with no number":              `{"id": 9001, "pull_request_url": "https://api.github.com/repos/octo/api/pulls/x"}`,
		"a link that is a path":              `{"id": 9001, "pull_request_url": "/repos/octo/api/pulls/7"}`,
		"a link to a number that is no item": `{"id": 9001, "pull_request_url": "https://api.github.com/repos/octo/api/pulls/0"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, c := newIdentityFixture(t, map[string]string{path: body})
			if n, err := NewIdentityReader().CommentSubject(context.Background(), c, "octo", "api", domain.GitHubReviewComment, 9001); err == nil {
				t.Errorf("CommentSubject = %d, want an error", n)
			}
		})
	}

	// A failed read is not cached.
	f, c := newIdentityFixture(t, map[string]string{})
	r := NewIdentityReader()
	if _, err := r.CommentSubject(context.Background(), c, "octo", "api", domain.GitHubReviewComment, 9001); err == nil {
		t.Fatal("a missing comment answered a subject")
	}
	f.mu.Lock()
	f.bodies[path] = `{"id": 9001, "pull_request_url": "https://api.github.com/repos/octo/api/pulls/7"}`
	f.mu.Unlock()
	if n, err := r.CommentSubject(context.Background(), c, "octo", "api", domain.GitHubReviewComment, 9001); err != nil || n != 7 {
		t.Errorf("CommentSubject after the comment appeared = %d, %v; want 7", n, err)
	}
	if _, err := r.CommentSubject(context.Background(), c, "octo", "api", "other", 9001); err == nil {
		t.Error("a numbering outside the two answered a subject")
	}
}

// nodeAnswers is a Querier that answers NodeSubject's query from a node id →
// response table, counting the queries, and fails for an id it does not hold.
type nodeAnswers struct {
	mu      sync.Mutex
	answers map[string]string
	queries int
}

func (q *nodeAnswers) PostGraphQL(_ context.Context, query string, variables map[string]any) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries++
	if !strings.Contains(query, "query NodeSubject(") {
		return nil, errors.New("not the node query")
	}
	id, _ := variables["id"].(string)
	body, ok := q.answers[id]
	if !ok {
		return nil, errors.New("no answer")
	}
	return []byte(body), nil
}

// TestIdentityReader_NodeSubject: a node id answers the pull request or issue
// it names, or the one the comment it names is on, in the repository GitHub
// names now; any other kind of node answers nothing. Every answer is read
// once per reader.
func TestIdentityReader_NodeSubject(t *testing.T) {
	q := &nodeAnswers{answers: map[string]string{
		"PR_kw7":   `{"data":{"node":{"__typename":"PullRequest","number":7,"repository":{"nameWithOwner":"octo/api"}}}}`,
		"I_kw9":    `{"data":{"node":{"__typename":"Issue","number":9,"repository":{"nameWithOwner":"octo/renamed"}}}}`,
		"IC_onPR":  `{"data":{"node":{"__typename":"IssueComment","pullRequest":{"number":7},"issue":{"number":7},"repository":{"nameWithOwner":"octo/api"}}}}`,
		"IC_onI":   `{"data":{"node":{"__typename":"IssueComment","pullRequest":null,"issue":{"number":9},"repository":{"nameWithOwner":"octo/api"}}}}`,
		"PRRC_kw":  `{"data":{"node":{"__typename":"PullRequestReviewComment","pullRequest":{"number":7},"repository":{"nameWithOwner":"octo/api"}}}}`,
		"U_kwUser": `{"data":{"node":{"__typename":"User"}}}`,
	}}
	r := NewIdentityReader()
	want := map[string]NodeSubject{
		"PR_kw7":   {Owner: "octo", Repo: "api", Number: 7},
		"I_kw9":    {Owner: "octo", Repo: "renamed", Number: 9},
		"IC_onPR":  {Owner: "octo", Repo: "api", Number: 7},
		"IC_onI":   {Owner: "octo", Repo: "api", Number: 9},
		"PRRC_kw":  {Owner: "octo", Repo: "api", Number: 7},
		"U_kwUser": {},
	}
	for range 2 {
		for id, w := range want {
			if got, err := r.NodeSubject(context.Background(), q, id); err != nil || got != w {
				t.Errorf("NodeSubject(%s) = %+v, %v; want %+v", id, got, err, w)
			}
		}
	}
	if q.queries != len(want) {
		t.Errorf("queries = %d, want one per node id", q.queries)
	}
}

func TestIdentityReader_NodeSubjectFailuresAreNotCached(t *testing.T) {
	q := &nodeAnswers{answers: map[string]string{
		"PR_null":     `{"data":{"node":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a node"}]}`,
		"PR_noRepo":   `{"data":{"node":{"__typename":"PullRequest","number":7}}}`,
		"PR_noNum":    `{"data":{"node":{"__typename":"PullRequest","repository":{"nameWithOwner":"octo/api"}}}}`,
		"PR_badRepo":  `{"data":{"node":{"__typename":"PullRequest","number":7,"repository":{"nameWithOwner":"octo"}}}}`,
		"IC_noParent": `{"data":{"node":{"__typename":"IssueComment","pullRequest":null,"issue":null,"repository":{"nameWithOwner":"octo/api"}}}}`,
		"PR_garbled":  `not json`,
	}}
	r := NewIdentityReader()
	for id := range q.answers {
		if got, err := r.NodeSubject(context.Background(), q, id); err == nil {
			t.Errorf("NodeSubject(%s) = %+v, want an error", id, got)
		}
	}
	if _, err := r.NodeSubject(context.Background(), q, "PR_unanswered"); err == nil {
		t.Error("a failed query answered a subject")
	}
	if _, err := r.NodeSubject(context.Background(), q, ""); err == nil {
		t.Error("an empty node id answered a subject")
	}
	before := q.queries
	for id := range q.answers {
		_, _ = r.NodeSubject(context.Background(), q, id)
	}
	if q.queries != before+len(q.answers) {
		t.Errorf("queries after the failures = %d, want each asked again", q.queries-before)
	}
}

// TestTokenQuerier: the querier posts the document and its variables with the
// credential, answers the body of a response with data, errors beside it
// included, and fails a non-2xx or a response with no data.
func TestTokenQuerier(t *testing.T) {
	var sawAuth, sawBody string
	status, answer := http.StatusOK, `{"data":{"node":{"__typename":"User"}},"errors":[{"type":"FORBIDDEN","message":"x"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/graphql" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	q := NewTokenQuerier(srv.URL+"/api/graphql", func(context.Context) (string, error) { return "ghs_real", nil }, nil)

	body, err := q.PostGraphQL(context.Background(), "query Q($id: ID!) { node(id: $id) { id } }", map[string]any{"id": "PR_x"})
	if err != nil || string(body) != answer {
		t.Fatalf("PostGraphQL = %q, %v; want the body with data", body, err)
	}
	if sawAuth != "token ghs_real" {
		t.Errorf("authorization = %q, want the credential", sawAuth)
	}
	var sent struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(sawBody), &sent); err != nil || !strings.HasPrefix(sent.Query, "query Q(") || sent.Variables["id"] != "PR_x" {
		t.Errorf("sent %q, want the document and its variables", sawBody)
	}

	for name, c := range map[string]struct {
		status int
		answer string
	}{
		"no data":   {http.StatusOK, `{"errors":[{"type":"RATE_LIMITED","message":"spent"}]}`},
		"null data": {http.StatusOK, `{"data":null}`},
		"not json":  {http.StatusOK, `<html>`},
		"a non-2xx": {http.StatusBadGateway, `{"message":"bad gateway"}`},
	} {
		status, answer = c.status, c.answer
		if body, err := q.PostGraphQL(context.Background(), "query Q { viewer { login } }", nil); err == nil {
			t.Errorf("%s: PostGraphQL = %q, want an error", name, body)
		}
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
