package ghinjector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
)

// gatedRun is one injector wired the way production wires it: the refusal
// policy on, a decision hook that records what it was asked and answers no, and
// an upstream that records whatever still reached it.
//
// Every assertion in this file is one of two kinds — the refused request never
// reached the upstream and left a decision, or the transiting request did reach
// it and left none. The upstream recorder is what makes the first kind an
// observation rather than an inference from a status code.
type gatedRun struct {
	client   *http.Client
	host     string
	upstream *httptest.Server

	// writes receives the ordinary write audit. It is wired on every run in
	// this file so that "a refused request leaves the refusal and nothing else"
	// is asserted rather than assumed — one request, one row, in both
	// directions.
	writes chan ObservedWrite

	mu        sync.Mutex
	forwarded []string
	decisions []gateDecision
}

// gateDecision is one call the injector made to the decision hook — the same
// pair the relay carries, and the same pair the audit row is built from.
type gateDecision struct {
	req ghwrite.Request
	ref ghwrite.Refusal
}

func newGatedRun(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *gatedRun {
	t.Helper()
	g := &gatedRun{writes: make(chan ObservedWrite, 4)}
	g.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.forwarded = append(g.forwarded, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(g.upstream.Close)

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:     g.upstream.URL,
		Cert:         cert,
		TokenSource:  func(context.Context) (string, error) { return "ghs_realtoken", nil },
		ObserveWrite: func(_ context.Context, w ObservedWrite) { g.writes <- w },
		AuthorizeWrite: func(_ context.Context, req ghwrite.Request, ref ghwrite.Refusal) bool {
			g.mu.Lock()
			g.decisions = append(g.decisions, gateDecision{req: req, ref: ref})
			g.mu.Unlock()
			// What both production wirings answer for a gated shape. There is
			// no authorized case in V1.
			return false
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert PEM to pool failed")
	}
	g.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	g.host = addr
	return g
}

// ok is the ordinary upstream: a 200 with an empty JSON object, for the cases
// where what the upstream returns is beside the point.
func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func (g *gatedRun) do(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://"+g.host+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

func (g *gatedRun) snap() (forwarded []string, decisions []gateDecision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.forwarded...), append([]gateDecision(nil), g.decisions...)
}

// assertRefused is the shape every gated case must have: a 403 the model can
// read, nothing forwarded, and exactly one decision carrying the reason the
// audit row will be filed under.
func (g *gatedRun) assertRefused(t *testing.T, resp *http.Response, wantReason string) gateDecision {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var decoded refusalBody
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("refusal body did not parse as JSON (%v): %s", err, raw)
	}
	if decoded.Message == "" || decoded.TriageFactory.Policy == "" || decoded.TriageFactory.NextStep == "" {
		t.Errorf("refusal body = %s, want a message, a policy and a next step", raw)
	}
	if decoded.TriageFactory.Reason != wantReason {
		t.Errorf("refusal reason = %q, want %q", decoded.TriageFactory.Reason, wantReason)
	}

	forwarded, decisions := g.snap()
	if len(forwarded) != 0 {
		t.Fatalf("a refused request reached GitHub: %q", forwarded)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions = %d, want exactly one — the refusal is the row", len(decisions))
	}
	if decisions[0].ref.Reason != wantReason {
		t.Errorf("decision reason = %q, want %q", decisions[0].ref.Reason, wantReason)
	}
	select {
	case w := <-g.writes:
		t.Errorf("a refused request also left a write-audit row: %+v", w)
	default:
	}
	return decisions[0]
}

// TestGate_MergeTransitsWhateverTheSpelling pins a deliberate absence.
//
// Merging is the obvious third family and it is not gated: some missions are
// legitimately for landing work, and unlike a review or a new repository there
// is no dominating alternative to redirect to. The control that stands is the
// delegated runtime's merge question, which interrupts the first attempt and
// asks the model to quote its authorization — and which promises that a
// re-issue proceeds. This is what makes that promise true.
//
// Pinned rather than left implicit because the four spellings below are exactly
// the ones a gated family would sweep up by act, so a later change that adds
// pr_merged to the table would silently break the question's contract. If that
// is ever the intent, this test is where the intent gets stated.
func TestGate_MergeTransitsWhateverTheSpelling(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			// What `gh pr merge` sends.
			name:   "gh pr merge",
			method: http.MethodPost,
			path:   "/api/graphql",
			body: `{"query":"mutation MergePullRequest($input:MergePullRequestInput!)` +
				`{mergePullRequest(input:$input){clientMutationId}}",` +
				`"variables":{"input":{"pullRequestId":"PR_kwABC"}}}`,
		},
		{
			// What `gh pr merge --auto` sends: a distinct mutation, not a flag on
			// the one above. It follows the merge rather than leading it — arming
			// a merge cannot be the more dangerous of the two, so gating it while
			// the merge itself transits would refuse the cautious spelling and
			// permit the direct one.
			name:   "gh pr merge --auto",
			method: http.MethodPost,
			path:   "/api/graphql",
			body: `{"query":"mutation EnableAutoMerge($input:EnablePullRequestAutoMergeInput!)` +
				`{enablePullRequestAutoMerge(input:$input){clientMutationId}}"}`,
		},
		{
			name:   "gh api REST merge",
			method: http.MethodPut,
			path:   "/api/v3/repos/acme/widgets/pulls/7/merge",
			body:   `{"merge_method":"squash"}`,
		},
		{
			name:   "curl of the same shape",
			method: http.MethodPut,
			path:   "/repos/acme/widgets/pulls/7/merge",
			body:   `{"merge_method":"merge"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedRun(t, ok)
			resp := g.do(t, tc.method, tc.path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want the upstream's 200 — merging is not gated here", resp.StatusCode)
			}
			forwarded, decisions := g.snap()
			if len(forwarded) != 1 {
				t.Fatalf("forwarded = %q, want the merge to reach GitHub", forwarded)
			}
			if len(decisions) != 0 {
				t.Errorf("decisions = %+v, want none — an ungated act is never asked about", decisions)
			}
		})
	}
}

// TestGate_ReviewAndRepoCreationAreRefused covers the two gated families, in
// each of the spellings the pinned gh and a hand-written call produce.
//
// The two repository REST collections are the reason the gate cannot key on the
// classifier alone: they POST outside /repos/{owner}/{repo}/, which is the
// anchor the whole REST table hangs off, so there is nothing there to classify.
func TestGate_ReviewAndRepoCreationAreRefused(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		reason string
	}{
		{
			name: "gh pr review", method: http.MethodPost, path: "/api/graphql",
			body:   `{"query":"mutation($input:AddPullRequestReviewInput!){addPullRequestReview(input:$input){clientMutationId}}"}`,
			reason: ghwrite.GateReasonReview,
		},
		{
			name: "submitting a pending review by hand", method: http.MethodPost, path: "/api/graphql",
			body:   `{"query":"mutation{submitPullRequestReview(input:{pullRequestReviewId:\"PRR_1\",event:APPROVE}){clientMutationId}}"}`,
			reason: ghwrite.GateReasonReview,
		},
		{
			// The one gated mutation the classifier deliberately does not name:
			// it stages a thread on a pending review, which every audit verb
			// would overstate. Gated by name instead, and the gate is why no
			// verb is needed — a refused mutation never becomes an act.
			name: "staging a review thread", method: http.MethodPost, path: "/api/graphql",
			body:   `{"query":"mutation{addPullRequestReviewThread(input:{path:\"a.go\",line:1,body:\"x\"}){clientMutationId}}"}`,
			reason: ghwrite.GateReasonReview,
		},
		{
			name: "the REST review post", method: http.MethodPost, path: "/api/v3/repos/acme/widgets/pulls/7/reviews",
			body: `{"event":"APPROVE"}`, reason: ghwrite.GateReasonReview,
		},
		{
			name: "gh repo create", method: http.MethodPost, path: "/api/graphql",
			body:   `{"query":"mutation{createRepository(input:{name:\"w\",ownerId:\"O_1\"}){repository{url}}}"}`,
			reason: ghwrite.GateReasonRepoCreate,
		},
		{
			name: "gh repo create --template", method: http.MethodPost, path: "/api/graphql",
			body:   `{"query":"mutation{cloneTemplateRepository(input:{name:\"w\"}){repository{url}}}"}`,
			reason: ghwrite.GateReasonRepoCreate,
		},
		{
			name: "the personal repo collection", method: http.MethodPost, path: "/api/v3/user/repos",
			body: `{"name":"widgets"}`, reason: ghwrite.GateReasonRepoCreate,
		},
		{
			name: "the org repo collection", method: http.MethodPost, path: "/api/v3/orgs/acme/repos",
			body: `{"name":"widgets"}`, reason: ghwrite.GateReasonRepoCreate,
		},
		{
			// This one the classifier DOES name, since it addresses a repository
			// and so has coordinates: instantiating a template makes a new
			// repository out of an existing one.
			name: "the template endpoint", method: http.MethodPost, path: "/api/v3/repos/acme/tmpl/generate",
			body: `{"name":"widgets","owner":"acme"}`, reason: ghwrite.GateReasonRepoCreate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedRun(t, ok)
			g.assertRefused(t, g.do(t, tc.method, tc.path, tc.body), tc.reason)
		})
	}
}

// TestGate_UnnameableWritesAreRefused is the evasion set. Each case is a way of
// preventing the act from being NAMED, which a gate keyed on the act's name
// would otherwise be walked straight through by.
//
// The first two are named rather than blocked and gate on their merits — an
// alias resolves to the field it hides, a resolvable spread is followed into
// its definition — so they arrive at the review family like any other review.
// The rest cannot be named at all and are refused on exactly that ground.
//
// The hidden act is a REVIEW rather than a merge because merging is not gated
// (see TestGate_MergeTransitsWhateverTheSpelling): an evasion test has to hide
// something that would otherwise be refused, or it proves nothing about the
// evasion. The undecidable cases below are shape-independent by construction —
// nobody knows what they were — so their documents still say merge, which is
// exactly the point of refusing them.
func TestGate_UnnameableWritesAreRefused(t *testing.T) {
	padded := `{"query":"mutation($input:MergePullRequestInput!){mergePullRequest(input:$input){clientMutationId}}",` +
		`"variables":{"input":{"pullRequestId":"PR_1","commitBody":"` + strings.Repeat("x", maxRequestBody) + `"}}}`

	cases := []struct {
		name       string
		body       string
		reason     string
		unreadable string
	}{
		{
			name:   "an aliased review",
			body:   `{"query":"mutation{x: addPullRequestReview(input:{pullRequestId:\"PR_1\"}){clientMutationId}}"}`,
			reason: ghwrite.GateReasonReview,
		},
		{
			name: "a review behind a fragment spread",
			body: `{"query":"mutation{...M} fragment M on Mutation{addPullRequestReview(input:{pullRequestId:\"PR_1\"})` +
				`{clientMutationId}}"}`,
			reason: ghwrite.GateReasonReview,
		},
		{
			name:       "a write behind an undefined fragment",
			body:       `{"query":"mutation{...Missing}"}`,
			reason:     ghwrite.GateReasonUnreadable,
			unreadable: ghwrite.GraphQLUnresolvedFragment,
		},
		{
			name:       "a write padded past the buffer cap",
			body:       padded,
			reason:     ghwrite.GateReasonUnreadable,
			unreadable: ghwrite.GraphQLOverCap,
		},
		{
			name: "several operations and no operationName",
			body: `{"query":"mutation A{mergePullRequest(input:{pullRequestId:\"PR_1\"}){clientMutationId}} ` +
				`mutation B{addComment(input:{subjectId:\"PR_1\",body:\"hi\"}){clientMutationId}}"}`,
			reason:     ghwrite.GateReasonUnreadable,
			unreadable: ghwrite.GraphQLAmbiguousOperation,
		},
		{
			// The prescreen runs on the DECODED document, so spelling the
			// keyword with JSON escapes hides it from a scan of the bytes and
			// from nothing else.
			name:   "the mutation keyword written as a JSON escape",
			body:   `{"query":"\u006dutation{addPullRequestReview(input:{pullRequestId:\"PR_1\"}){clientMutationId}}"}`,
			reason: ghwrite.GateReasonReview,
		},
		{
			// A selection wider than the reader keeps: without the truncation
			// reason, padding the front of the selection set would push the
			// gated field out of view entirely.
			name: "a write pushed past the field cap",
			body: `{"query":"mutation{a:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`b:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`c:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`d:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`e:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`f:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`g:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`h:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`i:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`j:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`k:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`l:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`m:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`n:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`o:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`p:addComment(input:{subjectId:\"1\",body:\"x\"}){clientMutationId} ` +
				`addPullRequestReview(input:{pullRequestId:\"PR_1\"}){clientMutationId}}"}`,
			reason:     ghwrite.GateReasonUnreadable,
			unreadable: ghwrite.GraphQLTooManyFields,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedRun(t, ok)
			got := g.assertRefused(t, g.do(t, http.MethodPost, "/api/graphql", tc.body), tc.reason)
			if got.ref.Unreadable != tc.unreadable {
				t.Errorf("unreadable reason = %q, want %q — the log has to tell a refused merge "+
					"apart from something we could not read", got.ref.Unreadable, tc.unreadable)
			}
		})
	}
}

// TestGate_RequestUnreadIsRefused is the last member of the undecidable band:
// the body's own read failed partway through, so there was never a document.
// Driven at the handler rather than over TLS, because a client that dies
// mid-send is not something an http.Client will do on request.
func TestGate_RequestUnreadIsRefused(t *testing.T) {
	var decided *gateDecision
	srv, err := New(Config{
		Upstream:    "https://api.github.com",
		Cert:        tls.Certificate{},
		TokenSource: func(context.Context) (string, error) { return "ghs_x", nil },
		AuthorizeWrite: func(_ context.Context, req ghwrite.Request, ref ghwrite.Refusal) bool {
			decided = &gateDecision{req: req, ref: ref}
			return false
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/graphql",
		&failingBody{prefix: `{"query":"mutation{mergePullRequest`})
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a body that never arrived", rec.Code)
	}
	if decided == nil {
		t.Fatal("no decision was made for an unreadable request")
	}
	if decided.ref.Unreadable != ghwrite.GraphQLRequestUnread {
		t.Errorf("unreadable reason = %q, want %q", decided.ref.Unreadable, ghwrite.GraphQLRequestUnread)
	}
}

// TestGate_UngatedWriteStillTransitsAndAudits is what proves this is a SHAPE
// gate and not a blanket block. Without it the suite would pass just as well
// against a proxy that refused everything.
func TestGate_UngatedWriteStillTransitsAndAudits(t *testing.T) {
	const commentURL = "https://github.com/acme/widgets/pull/841#issuecomment-7"
	g := newGatedRun(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"addComment":{"commentEdge":{"node":{"url":"` + commentURL + `"}}}}}`))
	})
	resp := g.do(t, http.MethodPost, "/api/graphql",
		`{"query":"mutation($input:AddCommentInput!){addComment(input:$input){commentEdge{node{url}}}}",`+
			`"variables":{"input":{"subjectId":"PR_1","body":"pushed a fix"}}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the upstream's 200 — an ungated write must transit", resp.StatusCode)
	}

	forwarded, decisions := g.snap()
	if len(forwarded) != 1 {
		t.Fatalf("forwarded = %q, want the comment to reach GitHub", forwarded)
	}
	if len(decisions) != 0 {
		t.Errorf("decisions = %+v, want none — an ungated write is never asked about", decisions)
	}

	select {
	case w := <-g.writes:
		shape, named := ghwrite.Resolve(w)
		if !named || !w.Succeeded() {
			t.Fatalf("audit = %+v, want a resolved successful write", w)
		}
		if shape.Action != "comment_posted" {
			t.Errorf("audited act = %q, want comment_posted — its own semantic row", shape.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an ungated write left no audit row")
	}
}

// TestGate_ReadsAreUntouched is the exemption that must not regress. The three
// below are what `gh pr view`, `gh pr diff`, and a hand-written
// `gh api graphql` query put on the wire: none is gated, none is asked about,
// and all three transit.
func TestGate_ReadsAreUntouched(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"gh pr view", http.MethodPost, "/api/graphql",
			`{"query":"query($owner:String!){repository(owner:$owner,name:\"w\"){pullRequest(number:7){title}}}",` +
				`"variables":{"owner":"acme"}}`},
		{"gh pr diff", http.MethodGet, "/api/v3/repos/acme/widgets/pulls/7", ""},
		{"a hand-written graphql query", http.MethodPost, "/api/graphql",
			`{"query":"query{viewer{login}}"}`},
		{
			// A read whose text merely MENTIONS a mutation, which the keyword
			// prescreen lets through to the document reader and the reader then
			// resolves as a query. Refusing it would be refusing ordinary work.
			"a query that mentions mutations", http.MethodPost, "/api/graphql",
			`{"query":"query{__type(name:\"Mutation\"){fields{name}}}"}`,
		},
		{
			// The same at the REST layer: a GET on the merge endpoint asks
			// whether a PR is merged. Reading is not merging.
			"checking whether a PR is merged", http.MethodGet,
			"/api/v3/repos/acme/widgets/pulls/7/merge", "",
		},
		{
			// And the repository collections, which are also ordinary lists.
			"listing repositories", http.MethodGet, "/api/v3/user/repos", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedRun(t, ok)
			resp := g.do(t, tc.method, tc.path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want the upstream's 200 — reads are never refused", resp.StatusCode)
			}
			forwarded, decisions := g.snap()
			if len(forwarded) != 1 {
				t.Fatalf("forwarded = %q, want the read to reach GitHub", forwarded)
			}
			if len(decisions) != 0 {
				t.Errorf("decisions = %+v, want none — a read is never a gated shape", decisions)
			}
		})
	}
}

// TestGate_FailsClosedWithoutADecisionSource: a gated shape whose decision
// cannot be obtained is refused. Otherwise a wedged relay — or a wiring mistake
// that left the hook nil — becomes the way past the gate.
func TestGate_FailsClosedWithoutADecisionSource(t *testing.T) {
	forwarded := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cert, certPEM, _ := GenerateCert("127.0.0.1")
	srv, err := New(Config{
		Upstream: upstream.URL, Cert: cert,
		TokenSource: func(context.Context) (string, error) { return "ghs_x", nil },
		// No AuthorizeWrite at all.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/api/v3/repos/acme/widgets/pulls/7/reviews",
		strings.NewReader(`{"event":"APPROVE"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 with no decision source wired", resp.StatusCode)
	}
	if forwarded {
		t.Error("a gated write transited with nothing to authorize it")
	}
}

// TestGate_RefusedRequestNeverTouchesTheCredential: the refusal happens before
// the credential is resolved, so a gated request cannot spend a token-source
// call — let alone reach GitHub holding one.
func TestGate_RefusedRequestNeverTouchesTheCredential(t *testing.T) {
	resolved := 0
	cert, certPEM, _ := GenerateCert("127.0.0.1")
	srv, err := New(Config{
		Upstream: "https://api.github.com", Cert: cert,
		TokenSource: func(context.Context) (string, error) {
			resolved++
			return "", errors.New("the credential pipeline must not be reached")
		},
		AuthorizeWrite: func(context.Context, ghwrite.Request, ghwrite.Refusal) bool { return false },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/api/v3/repos/acme/widgets/pulls/7/reviews",
		strings.NewReader(`{"event":"APPROVE"}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (not the 502 a credential failure would give)", resp.StatusCode)
	}
	if resolved != 0 {
		t.Errorf("the credential was resolved %d times for a refused request, want 0", resolved)
	}
}
