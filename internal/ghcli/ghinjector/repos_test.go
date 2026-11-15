package ghinjector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
)

// repoUpstream is a fake GitHub that answers the repository reads from a
// table and every other request with a 200, recording the agent's requests it
// was forwarded apart from the lookups the gate made of its own.
type repoUpstream struct {
	srv *httptest.Server

	mu        sync.Mutex
	forwarded []string
	lookups   []string
}

// agentHeader marks a request the test sent as the agent, which the proxy
// forwards and the gate's own lookups do not carry.
const agentHeader = "X-Test-Agent"

// The fixture's repositories: the run's own, a private one outside the run,
// a public one outside it, and the run's own under a name it had before.
var repoLookups = map[string]struct {
	status int
	body   string
}{
	"/repos/octo/repo":     {200, `{"id": 1, "full_name": "octo/repo", "private": true}`},
	"/repos/octo/old-name": {200, `{"id": 1, "full_name": "octo/repo", "private": true}`},
	"/repos/acme/payments": {200, `{"id": 2, "full_name": "acme/payments", "private": true}`},
	"/repos/acme/lib":      {200, `{"id": 3, "full_name": "acme/lib", "private": false}`},
	"/repos/acme/gone":     {404, `{"message": "Not Found"}`},
	"/repos/acme/broken":   {500, `{"message": "boom"}`},
}

func newRepoUpstream(t *testing.T) *repoUpstream {
	t.Helper()
	u := &repoUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		if r.Header.Get(agentHeader) != "" {
			u.forwarded = append(u.forwarded, r.Method+" "+r.URL.Path)
		} else {
			u.lookups = append(u.lookups, r.Method+" "+r.URL.Path)
		}
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if answer, ok := repoLookups[r.URL.Path]; ok {
				w.WriteHeader(answer.status)
				_, _ = w.Write([]byte(answer.body))
				return
			}
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// requests is what the upstream was forwarded, and lookups what the gate
// asked it of its own.
func (u *repoUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.forwarded)
}

func (u *repoUpstream) madeLookups() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.lookups)
}

// gatedInjector is an injector with the repository gate wired over the
// set source returns, recording every denial.
type gatedInjector struct {
	srv *Server

	mu      sync.Mutex
	denials []DeniedRepository
}

func newGatedInjector(t *testing.T, upstream string, source RepositorySource) *gatedInjector {
	t.Helper()
	g := &gatedInjector{}
	srv, err := New(Config{
		Upstream:     upstream,
		TokenSource:  func(context.Context) (string, error) { return "ghs_realtoken", nil },
		Repositories: source,
		DenyRepository: func(_ context.Context, d DeniedRepository) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.denials = append(g.denials, d)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.srv = srv
	return g
}

// do sends one request through the injector's handler and returns the
// response.
func (g *gatedInjector) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := mustRequest(t, method, "https://ghe.test"+path, body)
	req.Header.Set(agentHeader, "1")
	g.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (g *gatedInjector) denied() []DeniedRepository {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.denials)
}

// ownSet is a run that works in octo/repo.
func ownSet(context.Context) ([]AuthorizedRepository, bool, error) {
	return []AuthorizedRepository{{Name: "octo/repo", ID: "1"}}, true, nil
}

// refusal decodes a gate refusal, failing the test when the response is not
// one.
func refusal(t *testing.T, rec *httptest.ResponseRecorder) repositoryRefusalBody {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the gate's 403\n%s", rec.Code, rec.Body.String())
	}
	var body repositoryRefusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestRepositoryGate_REST(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		allowed bool
		// lookup is whether the gate asks GitHub about the name.
		lookup bool
	}{
		{name: "the run's own repository", method: "GET", path: "/api/v3/repos/octo/repo/pulls/12", allowed: true},
		{name: "the run's own, another case", method: "GET", path: "/api/v3/repos/Octo/Repo/pulls/12", allowed: true},
		{name: "a write to the run's own", method: "POST", path: "/api/v3/repos/octo/repo/issues/7/comments", allowed: true},
		{name: "the run's own under a former name", method: "GET", path: "/api/v3/repos/octo/old-name/pulls/12", allowed: true, lookup: true},
		{name: "a private repository outside the run", method: "GET", path: "/api/v3/repos/acme/payments/pulls/12", lookup: true},
		{name: "a write to a private one", method: "PATCH", path: "/api/v3/repos/acme/payments/pulls/12", lookup: true},
		{name: "a dot segment into one", method: "GET", path: "/api/v3/repos/octo/repo/../../acme/payments/pulls/12", lookup: true},
		{name: "a read of a public repository outside the run", method: "GET", path: "/api/v3/repos/acme/lib/issues", allowed: true, lookup: true},
		{name: "a write to a public one", method: "POST", path: "/api/v3/repos/acme/lib/issues", lookup: true},
		{name: "a repository GitHub does not answer for", method: "GET", path: "/api/v3/repos/acme/gone", allowed: true, lookup: true},
		{name: "a lookup that fails", method: "GET", path: "/api/v3/repos/acme/broken/pulls/1", lookup: true},
		{name: "a name no repository has", method: "GET", path: "/api/v3/repos/acme/pay%20ments/pulls/1"},
		{name: "a path that names no repository", method: "GET", path: "/api/v3/search/issues?q=repo:acme/payments", allowed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newRepoUpstream(t)
			g := newGatedInjector(t, upstream.srv.URL, ownSet)
			rec := g.do(t, tc.method, tc.path, "")

			forwarded := upstream.requests()
			if tc.allowed {
				if rec.Code == http.StatusForbidden || len(forwarded) != 1 {
					t.Fatalf("status %d, upstream forwarded %v; want the request forwarded\n%s", rec.Code, forwarded, rec.Body.String())
				}
				if len(g.denied()) != 0 {
					t.Errorf("denials = %+v, want none", g.denied())
				}
			} else {
				refusal(t, rec)
				if len(forwarded) != 0 {
					t.Errorf("upstream forwarded %v, want nothing", forwarded)
				}
				if len(g.denied()) != 1 {
					t.Errorf("denials = %+v, want one", g.denied())
				}
			}
			if looked := len(upstream.madeLookups()) > 0; looked != tc.lookup {
				t.Errorf("lookup made = %v, want %v (%v)", looked, tc.lookup, upstream.madeLookups())
			}
		})
	}
}

func TestRepositoryGate_RefusalNamesTheRepository(t *testing.T) {
	upstream := newRepoUpstream(t)
	g := newGatedInjector(t, upstream.srv.URL, ownSet)

	body := refusal(t, g.do(t, "GET", "/api/v3/repos/acme/payments/pulls/12", ""))
	if body.TriageFactory.Repository != "acme/payments" || body.TriageFactory.Reason != repositoryNotInRun {
		t.Errorf("refusal = %+v", body.TriageFactory)
	}
	if !strings.Contains(body.Message, "acme/payments") || !strings.Contains(body.Message, "'workspace add acme/payments'") {
		t.Errorf("message %q does not name the repository and the way to add it", body.Message)
	}
	want := DeniedRepository{
		Repository: ghwrite.RepoRef{Owner: "acme", Repo: "payments"},
		Identity:   domain.GitHubIdentity{RepositoryID: "2", Repository: "acme/payments"},
		Method:     "GET",
		Path:       "/repos/acme/payments/pulls/12",
	}
	if got := g.denied(); len(got) != 1 || got[0] != want {
		t.Errorf("denials = %+v, want %+v", got, want)
	}

	// A public repository's refusal says reads are open.
	body = refusal(t, g.do(t, "POST", "/api/v3/repos/acme/lib/issues", `{"title":"x"}`))
	if !strings.Contains(body.Message, "public") {
		t.Errorf("message %q does not say the repository is readable", body.Message)
	}

	// One the gate could not settle says to retry, and is recorded as such.
	body = refusal(t, g.do(t, "GET", "/api/v3/repos/acme/broken", ""))
	if body.TriageFactory.Reason != repositoryUnresolved {
		t.Errorf("refusal = %+v, want unresolved", body.TriageFactory)
	}
	if got := g.denied(); len(got) != 3 || !got[2].Unresolved {
		t.Errorf("denials = %+v, want the third unresolved", got)
	}
}

func TestRepositoryGate_GraphQL(t *testing.T) {
	query := func(repos ...string) string {
		q := "query {"
		for i, repo := range repos {
			owner, name, _ := strings.Cut(repo, "/")
			q += " r" + string(rune('a'+i)) + `: repository(owner: "` + owner + `", name: "` + name + `") { pullRequest(number: 12) { id } }`
		}
		return q + " }"
	}
	cases := []struct {
		name    string
		body    string
		allowed bool
	}{
		{name: "the run's own", body: graphQLEnvelope(t, query("octo/repo"), nil), allowed: true},
		{
			name: "by variables",
			body: graphQLEnvelope(t, `query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) { id } }`,
				map[string]any{"owner": "acme", "repo": "payments"}),
		},
		{name: "beside the run's own", body: graphQLEnvelope(t, query("octo/repo", "acme/payments"), nil)},
		{name: "a public one", body: graphQLEnvelope(t, query("octo/repo", "acme/lib"), nil), allowed: true},
		{name: "a former name", body: graphQLEnvelope(t, query("octo/old-name"), nil), allowed: true},
		{name: "an owner that cannot be read", body: graphQLEnvelope(t, `{ repository(owner: ACME, name: "payments") { id } }`, nil)},
		{name: "no repository", body: graphQLEnvelope(t, `{ viewer { login } }`, nil), allowed: true},
		{
			// A mutation names its object by node id, so it is the write
			// gate's and the audit's, never this gate's.
			name:    "a mutation",
			body:    graphQLEnvelope(t, `mutation { addComment(input: {subjectId: "PR_elsewhere", body: "x"}) { clientMutationId } }`, nil),
			allowed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newRepoUpstream(t)
			g := newGatedInjector(t, upstream.srv.URL, ownSet)
			rec := g.do(t, "POST", graphqlPath, tc.body)
			forwarded := slices.Contains(upstream.requests(), "POST /graphql")
			if tc.allowed {
				if rec.Code != http.StatusOK || !forwarded {
					t.Fatalf("status %d, upstream saw %v; want the query forwarded\n%s", rec.Code, upstream.requests(), rec.Body.String())
				}
				return
			}
			refusal(t, rec)
			if forwarded {
				t.Errorf("upstream saw the refused query: %v", upstream.requests())
			}
			if len(g.denied()) != 1 {
				t.Errorf("denials = %+v, want one", g.denied())
			}
		})
	}
}

// A name is looked up once per run, and the set is read on every request, so
// a repository added mid-run is reachable on the next one.
func TestRepositoryGate_LooksUpOnceAndReadsTheSetPerRequest(t *testing.T) {
	upstream := newRepoUpstream(t)
	var mu sync.Mutex
	set := []AuthorizedRepository{{Name: "octo/repo", ID: "1"}}
	reads := 0
	g := newGatedInjector(t, upstream.srv.URL, func(context.Context) ([]AuthorizedRepository, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return slices.Clone(set), true, nil
	})

	for range 2 {
		refusal(t, g.do(t, "GET", "/api/v3/repos/acme/payments/pulls/12", ""))
	}
	mu.Lock()
	set = append(set, AuthorizedRepository{Name: "acme/payments", ID: "2"})
	mu.Unlock()
	if rec := g.do(t, "GET", "/api/v3/repos/acme/payments/pulls/12", ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d after the repository joined the set, want it forwarded\n%s", rec.Code, rec.Body.String())
	}
	// A repository the set holds by its id alone is matched through the
	// lookup of the name the request spells.
	mu.Lock()
	set = []AuthorizedRepository{{Name: "octo/repo", ID: "1"}, {ID: "3"}}
	mu.Unlock()
	if rec := g.do(t, "POST", "/api/v3/repos/acme/lib/issues", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d for a write to a repository the set holds by id\n%s", rec.Code, rec.Body.String())
	}

	if got, want := upstream.madeLookups(), []string{"GET /repos/acme/payments", "GET /repos/acme/lib"}; !slices.Equal(got, want) {
		t.Errorf("lookups = %v, want %v, each once", got, want)
	}
	if reads != 4 {
		t.Errorf("set reads = %d, want one per request", reads)
	}
}

func TestRepositoryGate_UngatedAndUnreadableSets(t *testing.T) {
	t.Run("no gate", func(t *testing.T) {
		upstream := newRepoUpstream(t)
		g := newGatedInjector(t, upstream.srv.URL, func(context.Context) ([]AuthorizedRepository, bool, error) {
			return nil, false, nil
		})
		if rec := g.do(t, "GET", "/api/v3/repos/acme/payments/pulls/12", ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d, want an ungated request forwarded", rec.Code)
		}
		if got := upstream.requests(); !slices.Equal(got, []string{"GET /repos/acme/payments/pulls/12"}) {
			t.Errorf("upstream forwarded %v, want the request", got)
		}
		if got := upstream.madeLookups(); len(got) != 0 {
			t.Errorf("lookups = %v, want none", got)
		}
	})

	t.Run("a set that cannot be read", func(t *testing.T) {
		upstream := newRepoUpstream(t)
		g := newGatedInjector(t, upstream.srv.URL, func(context.Context) ([]AuthorizedRepository, bool, error) {
			return nil, false, errors.New("no bundle")
		})
		body := refusal(t, g.do(t, "GET", "/api/v3/repos/octo/repo/pulls/12", ""))
		if body.TriageFactory.Reason != repositoryUnresolved {
			t.Errorf("refusal = %+v, want unresolved", body.TriageFactory)
		}
		if got := g.denied(); len(got) != 1 || !got[0].Unresolved || got[0].Name() != "octo/repo" {
			t.Errorf("denials = %+v, want one unresolved for octo/repo", got)
		}
		// A request that names no repository never reads the set.
		if rec := g.do(t, "GET", "/api/v3/user", ""); rec.Code != http.StatusOK {
			t.Errorf("status %d for a request naming no repository", rec.Code)
		}
	})
}
