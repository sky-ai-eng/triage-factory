package ghinjector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
)

// These tests drive the REAL pinned gh binary against a fake GitHub Enterprise
// endpoint standing behind a real injector, and assert what the injector
// observes. They exist because the synthetic unit tests below assert the
// injector against an assumption about what gh sends rather than against gh:
// the observation path originally watched two REST mutation paths, and gh's
// porcelain never sends either — every mutation it performs is GraphQL. Only a
// test that runs gh can pin that contract, so this one takes no opt-in switch,
// no root, no runsc, no network and no real repo. It skips only when the pinned
// binary is absent.
//
// pinnedGHBinary is where the image bakes the TF-pinned gh release; the same
// path internal/agentproc bind-mounts into the jail. On a dev box without it the
// tests skip; TF_TEST_GH_BINARY points them at a local copy.
const pinnedGHBinary = "/opt/tf/bin/gh"

func ghBinary(t testing.TB) string {
	t.Helper()
	if override := os.Getenv("TF_TEST_GH_BINARY"); override != "" {
		return override
	}
	if _, err := os.Stat(pinnedGHBinary); err != nil {
		t.Skipf("pinned gh binary not present at %s: %v", pinnedGHBinary, err)
	}
	return pinnedGHBinary
}

// ghePR is the coordinate set the fake endpoint reports for the created PR.
const (
	ghePROwner  = "octo"
	ghePRRepo   = "repo"
	ghePRNumber = 42
	ghePRNodeID = "PR_kwWireTest"
	ghePRURL    = "https://ghe.test/octo/repo/pull/42"
	// The link `gh pr comment`'s mutation answers with — a comment anchor on the
	// PR's own page, which is what lets the audit row locate an object the
	// request named only by node id.
	gheCommentURL = "https://ghe.test/octo/repo/pull/42#issuecomment-7"
)

// fakeGHE answers the handful of operations `gh pr create` / `gh pr view` need.
// The bodies are gh's own wire shapes: the create mutation's selection set is
// exactly `pullRequest { id url }` and the view query nests its result under
// data.repository.pullRequest — which is the discrimination the injector has to
// get right. createExtra is spliced into the create response as an extra
// top-level member, so a test can push that one response past the buffer cap
// without disturbing anything gh reads.
func fakeGHE(t *testing.T, createExtra string) (*httptest.Server, func() string) {
	t.Helper()
	var (
		mu   sync.Mutex
		auth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		query := string(body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/meta"):
			_, _ = w.Write([]byte(`{"verifiable_password_authentication":false}`))

		case !strings.HasSuffix(r.URL.Path, "/graphql"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))

		case strings.Contains(query, "RepositoryInfo"):
			_, _ = w.Write([]byte(`{"data":{"repository":{"id":"R_wire","name":"` + ghePRRepo +
				`","owner":{"login":"` + ghePROwner + `"},"defaultBranchRef":{"name":"main"},` +
				`"viewerPermission":"WRITE","hasIssuesEnabled":true,"mergeCommitAllowed":true,` +
				`"rebaseMergeAllowed":true,"squashMergeAllowed":true}}}`))

		case strings.Contains(query, "PullRequestCreate"):
			_, _ = w.Write([]byte(`{"data":{"createPullRequest":{"pullRequest":{"id":"` + ghePRNodeID +
				`","url":"` + ghePRURL + `"}}}` + createExtra + `}`))

		case strings.Contains(query, "PullRequestByNumber"):
			// One PR object answering every finder in these tests. gh asks for a
			// different field set per command (merge wants the merge state, close
			// wants the state), so the fixture is the union — a finder that gets
			// a field it did not ask for ignores it.
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"` + ghePRNodeID +
				`","number":42,"url":"` + ghePRURL + `","title":"T","state":"OPEN","body":"B",` +
				`"baseRefName":"main","headRefName":"feature","headRefOid":"deadbeef",` +
				`"isCrossRepository":false,"mergeStateStatus":"CLEAN","mergeable":"MERGEABLE",` +
				`"isInMergeQueue":false,"isMergeQueueEnabled":false,` +
				`"headRepositoryOwner":{"id":"U_octo","login":"` + ghePROwner + `"},` +
				`"commits":{"totalCount":1,"nodes":[{"commit":{"oid":"deadbeef"}}]},` +
				`"author":{"login":"someone"}}}}}`))

		case strings.Contains(query, "addComment"):
			_, _ = w.Write([]byte(`{"data":{"addComment":{"commentEdge":{"node":{"url":"` + gheCommentURL + `"}}}}}`))

		case strings.Contains(query, "closePullRequest"):
			_, _ = w.Write([]byte(`{"data":{"closePullRequest":{"pullRequest":{"id":"` + ghePRNodeID + `"}}}}`))

		case strings.Contains(query, "mergePullRequest"):
			_, _ = w.Write([]byte(`{"data":{"mergePullRequest":{"clientMutationId":null}}}`))

		case strings.Contains(query, "addReaction"):
			_, _ = w.Write([]byte(`{"data":{"addReaction":{"clientMutationId":null}}}`))

		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
}

// ghEnv stands an injector in front of upstream and returns the environment a
// gh invocation needs to reach it — the production shape: GH_HOST names the
// injector, the token in the environment is the per-run placeholder, and
// SSL_CERT_FILE points at a bundle carrying the injector's per-run leaf. The
// environment is built from scratch rather than inherited so a developer's real
// GH_TOKEN or gh config can't influence the result.
func ghEnv(t testing.TB, upstream string, observe func(context.Context, ObservedMutation)) []string {
	return ghEnvWithWrites(t, upstream, observe, nil)
}

// ghEnvWithWrites is ghEnv with the write-audit callback wired too — the half
// that answers "what did this run do", as opposed to Observe's "what exists".
func ghEnvWithWrites(t testing.TB, upstream string,
	observe func(context.Context, ObservedMutation),
	observeWrite func(context.Context, ObservedWrite)) []string {
	return ghEnvWithGate(t, upstream, observe, observeWrite, nil)
}

// ghEnvWithGate is the full harness: the injector's two observation callbacks
// plus the gate's decision hook, which is where a refused write leaves its one
// record. A nil hook keeps the production posture — every gated shape refused,
// nothing recorded — so the callers that only watch writes pass nil and read
// exactly as before.
func ghEnvWithGate(t testing.TB, upstream string,
	observe func(context.Context, ObservedMutation),
	observeWrite func(context.Context, ObservedWrite),
	authorize AuthorizeWrite) []string {
	t.Helper()
	const placeholder = "placeholder-wire-token"

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:       upstream + "/api/v3",
		IncomingToken:  placeholder,
		Cert:           cert,
		Observe:        observe,
		ObserveWrite:   observeWrite,
		AuthorizeWrite: authorize,
		TokenSource:    func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	home := t.TempDir()
	trust := filepath.Join(home, "trust.pem")
	if err := os.WriteFile(trust, TrustBundlePEM(certPEM), 0o600); err != nil {
		t.Fatalf("write trust bundle: %v", err)
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"GH_CONFIG_DIR=" + filepath.Join(home, "config", "gh"),
		"GH_HOST=" + host,
		"GH_ENTERPRISE_TOKEN=" + placeholder,
		"SSL_CERT_FILE=" + trust,
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
	}
}

// runGH invokes the pinned binary and fails the test if it errors, since a gh
// failure means the fixture drifted from what gh actually sends — exactly the
// signal these tests exist to produce.
func runGH(t testing.TB, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghBinary(t), args...)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// observations collects what the injector reported, from whichever goroutine
// the proxy called back on.
type observations struct {
	mu   sync.Mutex
	seen []ObservedMutation
}

func (o *observations) record(_ context.Context, m ObservedMutation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, m)
}

func (o *observations) snap() []ObservedMutation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ObservedMutation(nil), o.seen...)
}

// TestGHWire_PRCreateIsObserved is the contract this ticket exists to pin: the
// real gh creates a PR over GraphQL, and the injector records it from the
// response alone, with coordinates parsed out of the html url.
func TestGHWire_PRCreateIsObserved(t *testing.T) {
	upstream, upstreamAuth := fakeGHE(t, "")
	var obs observations
	env := ghEnv(t, upstream.URL, obs.record)

	out := runGH(t, env, "pr", "create", "-R", ghePROwner+"/"+ghePRRepo,
		"--head", "feature", "--base", "main", "--title", "T", "--body", "B")
	if !strings.Contains(out, ghePRURL) {
		t.Errorf("gh pr create output = %q, want the created PR url", out)
	}

	seen := obs.snap()
	if len(seen) != 1 {
		t.Fatalf("observations = %d (%+v), want exactly 1 pull_request", len(seen), seen)
	}
	m := seen[0]
	if m.Kind != "pull_request" || m.Owner != ghePROwner || m.Repo != ghePRRepo || m.Number != ghePRNumber {
		t.Errorf("observation = %+v, want pull_request %s/%s#%d", m, ghePROwner, ghePRRepo, ghePRNumber)
	}
	if m.NodeID != ghePRNodeID || m.URL != ghePRURL {
		t.Errorf("observation node id / url = %q / %q, want %q / %q", m.NodeID, m.URL, ghePRNodeID, ghePRURL)
	}
	// gh's selection set carries no title or refs; those stay empty for the
	// reconciler rather than being invented here.
	if m.Title != "" || m.Head != "" || m.Base != "" {
		t.Errorf("observation = %+v, want title/head/base empty on the GraphQL path", m)
	}
	if got := upstreamAuth(); got != "token ghs_realtoken" {
		t.Errorf("upstream Authorization = %q, want the injected real token", got)
	}
}

// TestGHWire_PRViewIsNotObserved is the query/mutation discrimination: reading a
// PR must record nothing. `gh pr view` returns a pullRequest object too — just
// nested under data.repository — so a generic walk of the response would
// attribute a PR the run only read as one it produced.
func TestGHWire_PRViewIsNotObserved(t *testing.T) {
	upstream, _ := fakeGHE(t, "")
	var obs observations
	env := ghEnv(t, upstream.URL, obs.record)

	out := runGH(t, env, "pr", "view", "42", "-R", ghePROwner+"/"+ghePRRepo)
	if !strings.Contains(out, ghePRURL) {
		t.Fatalf("gh pr view output = %q, want the PR url (fixture drift?)", out)
	}

	if seen := obs.snap(); len(seen) != 0 {
		t.Errorf("observations = %+v, want none for a read-only command", seen)
	}
}

// TestGHWire_OversizedCreateReachesGHIntact is the negative space: a create
// response past the buffer cap is delivered to gh byte-for-byte — gh still
// parses it and prints the url — and observation is what degrades, not the
// agent's response.
func TestGHWire_OversizedCreateReachesGHIntact(t *testing.T) {
	// An extra top-level member gh ignores, padded past the cap.
	pad, err := json.Marshal(strings.Repeat("x", maxObserveBody+4096))
	if err != nil {
		t.Fatalf("marshal pad: %v", err)
	}
	upstream, _ := fakeGHE(t, `,"extensions":{"pad":`+string(pad)+`}`)
	var obs observations
	env := ghEnv(t, upstream.URL, obs.record)

	out := runGH(t, env, "pr", "create", "-R", ghePROwner+"/"+ghePRRepo,
		"--head", "feature", "--base", "main", "--title", "T", "--body", "B")
	if !strings.Contains(out, ghePRURL) {
		t.Errorf("gh pr create output = %q, want the created PR url — the response was corrupted", out)
	}
	if seen := obs.snap(); len(seen) != 0 {
		t.Errorf("observations = %+v, want none for an over-cap response", seen)
	}
}

// writeObservations collects the audit records the injector produced, from
// whichever goroutine the proxy called back on.
type writeObservations struct {
	mu   sync.Mutex
	seen []ObservedWrite
}

func (o *writeObservations) record(_ context.Context, w ObservedWrite) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, w)
}

func (o *writeObservations) snap() []ObservedWrite {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ObservedWrite(nil), o.seen...)
}

// only returns the single record the run produced, failing if a command left
// none or more than one. One write, one row is the rule the whole epic rests
// on, and a porcelain command issues several requests — reads included — so
// "exactly one" is the assertion that matters.
func (o *writeObservations) only(t *testing.T, command string) ObservedWrite {
	t.Helper()
	seen := o.snap()
	if len(seen) != 1 {
		t.Fatalf("%s produced %d audit records (%+v), want exactly 1", command, len(seen), seen)
	}
	return seen[0]
}

// TestGHWire_PorcelainWritesAreAudited is this ticket's acceptance, run against
// the pinned binary rather than an assumption about it. Each of these commands
// performs its write as a GraphQL mutation — none of them touches a REST path
// — so before this every one of them completed leaving no trace at all.
func TestGHWire_PorcelainWritesAreAudited(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		mutation string
		action   string
		target   string
		url      string
	}{
		{
			name:     "pr comment",
			args:     []string{"pr", "comment", "42", "-R", ghePROwner + "/" + ghePRRepo, "--body", "a reply"},
			mutation: "addComment",
			action:   domain.ActionCommentPosted,
			// The request named only a node id; the response located it.
			target: ghePROwner + "/" + ghePRRepo + "#" + strconv.Itoa(ghePRNumber),
			url:    gheCommentURL,
		},
		{
			name:     "pr close",
			args:     []string{"pr", "close", "42", "-R", ghePROwner + "/" + ghePRRepo},
			mutation: "closePullRequest",
			action:   domain.ActionPRClosed,
			// gh selects only the id back, so the node id is all this row can
			// honestly carry.
			target: ghePRNodeID,
		},
		{
			name:     "pr merge",
			args:     []string{"pr", "merge", "42", "-R", ghePROwner + "/" + ghePRRepo, "--merge"},
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
			target:   ghePRNodeID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, _ := fakeGHE(t, "")
			var writes writeObservations
			env := ghEnvWithWrites(t, upstream.URL, nil, writes.record)

			runGH(t, env, tc.args...)

			w := writes.only(t, "gh "+strings.Join(tc.args, " "))
			if w.GraphQL == nil {
				t.Fatalf("audit record = %+v, want the request's own facts attached", w)
			}
			if got := w.GraphQL.Mutation(); got != tc.mutation {
				t.Errorf("mutation = %q, want %q — the fixture or the table has drifted from gh", got, tc.mutation)
			}
			shape, ok := ghwrite.Resolve(w)
			if !ok || !w.Succeeded() {
				t.Fatalf("record = %+v resolved to %+v (ok=%v), want a classified success", w, shape, ok)
			}
			if shape.Action != tc.action {
				t.Errorf("action = %q, want %q", shape.Action, tc.action)
			}
			if shape.Target() != tc.target {
				t.Errorf("target = %q, want %q", shape.Target(), tc.target)
			}
			if w.URL != tc.url {
				t.Errorf("url = %q, want %q", w.URL, tc.url)
			}
		})
	}
}

// TestGHWire_HandWrittenMutationIsAudited covers the escape hatch: an agent
// that writes its own `gh api graphql` call, with the mutation inline and
// anonymous rather than in the shape gh's own commands emit.
func TestGHWire_HandWrittenMutationIsAudited(t *testing.T) {
	upstream, _ := fakeGHE(t, "")
	var writes writeObservations
	env := ghEnvWithWrites(t, upstream.URL, nil, writes.record)

	runGH(t, env, "api", "graphql", "-f",
		`query=mutation{addReaction(input:{subjectId:"`+ghePRNodeID+`",content:THUMBS_UP}){clientMutationId}}`)

	w := writes.only(t, "gh api graphql")
	shape, ok := ghwrite.Resolve(w)
	if !ok || shape.Action != domain.ActionReactionAdded {
		t.Fatalf("record = %+v resolved to %+v (ok=%v), want reaction_added", w, shape, ok)
	}
	// The node id rode in the document's own argument rather than the variables,
	// so nothing located the object. The act is still named, which is the point:
	// target resolution never gates the record.
	if shape.Target() != "" {
		t.Errorf("target = %q, want empty — this request disclosed no variables to read", shape.Target())
	}
}

// TestGHWire_ReadsAreNotAudited is the acceptance's other half and the reason
// the prescreen exists. gh issues several requests per command — repo lookups,
// finder queries, schema introspection — and none of them may leave a row.
func TestGHWire_ReadsAreNotAudited(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "view", "42", "-R", ghePROwner + "/" + ghePRRepo},
		{"pr", "diff", "42", "-R", ghePROwner + "/" + ghePRRepo},
		{"api", "graphql", "-f", `query=query{viewer{login}}`},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			upstream, _ := fakeGHE(t, "")
			var writes writeObservations
			env := ghEnvWithWrites(t, upstream.URL, nil, writes.record)

			// A read's exit status is beside the point here (a diff against a
			// fixture with no patch is allowed to fail); what matters is that
			// nothing it sent was recorded as a write.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, ghBinary(t), args...)
			cmd.Env = env
			cmd.Dir = t.TempDir()
			_, _ = cmd.CombinedOutput()

			if seen := writes.snap(); len(seen) != 0 {
				t.Errorf("read left %d audit records (%+v), want none", len(seen), seen)
			}
		})
	}
}

// TestGHWire_PRCreateLeavesOneArtifactAndOneAction pins the boundary between
// the two callbacks on the command that fires both. The artifact answers "what
// exists" and the action answers "what this run did"; they are parallel records
// of one event, and neither may become two.
func TestGHWire_PRCreateLeavesOneArtifactAndOneAction(t *testing.T) {
	upstream, _ := fakeGHE(t, "")
	var (
		muts   observations
		writes writeObservations
	)
	env := ghEnvWithWrites(t, upstream.URL, muts.record, writes.record)

	runGH(t, env, "pr", "create", "-R", ghePROwner+"/"+ghePRRepo,
		"--head", "feature", "--base", "main", "--title", "T", "--body", "B")

	if seen := muts.snap(); len(seen) != 1 || seen[0].Kind != "pull_request" {
		t.Errorf("artifact observations = %+v, want exactly one pull_request", seen)
	}
	w := writes.only(t, "gh pr create")
	shape, ok := ghwrite.Resolve(w)
	if !ok || shape.Action != domain.ActionPRCreated {
		t.Fatalf("record = %+v resolved to %+v (ok=%v), want pr_created", w, shape, ok)
	}
	// The response carried the created PR's url, so the row addresses it the way
	// every other surface does — by number, not by the node id the request knew.
	if shape.Target() != ghePROwner+"/"+ghePRRepo+"#"+strconv.Itoa(ghePRNumber) {
		t.Errorf("target = %q, want the created PR's coordinates", shape.Target())
	}
}
