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

// fakeGHE answers the handful of operations the porcelain commands driven here
// need, in gh's own wire shapes. commentExtra is spliced into the comment
// mutation's response as an extra top-level member, so a test can push that
// one response past the buffer cap without disturbing anything gh reads. There
// is no arm for the create mutation: a `gh pr create` is refused before it is
// forwarded, and a fixture that answered it would be a claim that it can
// arrive.
func fakeGHE(t *testing.T, commentExtra string) (*httptest.Server, func() string) {
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
			_, _ = w.Write([]byte(`{"data":{"addComment":{"commentEdge":{"node":{"url":"` + gheCommentURL +
				`"}}}}` + commentExtra + `}`))

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

// ghEnvWithWrites stands an injector in front of upstream, with the write-audit
// callback wired, and returns the environment a gh invocation needs to reach it
// — the production shape: GH_HOST names the injector, the token in the
// environment is the per-run placeholder, and SSL_CERT_FILE points at a bundle
// carrying the injector's per-run leaf. The environment is built from scratch
// rather than inherited so a developer's real GH_TOKEN or gh config can't
// influence the result.
func ghEnvWithWrites(t testing.TB, upstream string,
	observeWrite func(context.Context, ObservedWrite)) []string {
	return ghEnvWithGate(t, upstream, observeWrite, nil)
}

// ghEnvWithGate is the full harness: the write-audit callback plus the gate's
// decision hook, which is where a refused write leaves its one record. A nil
// hook keeps the production posture — every gated shape refused, nothing
// recorded — so the callers that only watch writes pass nil and read exactly
// as before.
func ghEnvWithGate(t testing.TB, upstream string,
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

// runGHRefused invokes the pinned binary on a command the injector must refuse
// and returns its combined output; a command that succeeds is the failure,
// since it means the gate let the act through.
func runGHRefused(t testing.TB, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghBinary(t), args...)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gh %s succeeded, want it refused\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

// refusals collects the gate's decisions, from whichever goroutine the proxy
// called back on, answering no to each — the production posture.
type refusals struct {
	mu   sync.Mutex
	seen []ghwrite.Refusal
}

func (r *refusals) authorize(_ context.Context, _ ghwrite.Request, ref ghwrite.Refusal) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, ref)
	return false
}

func (r *refusals) snap() []ghwrite.Refusal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ghwrite.Refusal(nil), r.seen...)
}

// TestGHWire_PRCreateIsRefused pins the one door for opening a pull request:
// the real gh's `pr create` is stopped at the injector, the upstream never sees
// the mutation, and the agent reads a refusal that names the verb to use.
func TestGHWire_PRCreateIsRefused(t *testing.T) {
	upstream, upstreamAuth := fakeGHE(t, "")
	var (
		writes writeObservations
		gate   refusals
	)
	env := ghEnvWithGate(t, upstream.URL, writes.record, gate.authorize)

	out := runGHRefused(t, env, "pr", "create", "-R", ghePROwner+"/"+ghePRRepo,
		"--head", "feature", "--base", "main", "--title", "T", "--body", "B")
	if !strings.Contains(out, "pr create") {
		t.Errorf("gh pr create output = %q, want the refusal naming the verb to use instead", out)
	}

	seen := gate.snap()
	if len(seen) != 1 || seen[0].Reason != ghwrite.GateReasonPRCreate || seen[0].Mutation != "createPullRequest" {
		t.Fatalf("refusals = %+v, want exactly one for createPullRequest under %s", seen, ghwrite.GateReasonPRCreate)
	}
	// One request, one row: the refusal is the record, and nothing reached the
	// write audit because nothing was forwarded.
	if got := writes.snap(); len(got) != 0 {
		t.Errorf("write audit = %+v, want nothing for a refused create", got)
	}
	// gh reads the repository before it creates, and that read transits with
	// the real token; the create itself never does.
	if got := upstreamAuth(); got != "token ghs_realtoken" {
		t.Errorf("upstream Authorization = %q, want the injected real token on the reads that preceded the refusal", got)
	}
}

// TestGHWire_OversizedWriteResponseReachesGHIntact is the negative space: a
// write's response past the buffer cap is delivered to gh byte-for-byte — gh
// still parses it and prints the url — and the audit's detail is what degrades,
// not the agent's response.
func TestGHWire_OversizedWriteResponseReachesGHIntact(t *testing.T) {
	// An extra top-level member gh ignores, padded past the cap.
	pad, err := json.Marshal(strings.Repeat("x", maxBufferedBody+4096))
	if err != nil {
		t.Fatalf("marshal pad: %v", err)
	}
	upstream, _ := fakeGHE(t, `,"extensions":{"pad":`+string(pad)+`}`)
	var writes writeObservations
	env := ghEnvWithWrites(t, upstream.URL, writes.record)

	out := runGH(t, env, "pr", "comment", "42", "-R", ghePROwner+"/"+ghePRRepo, "--body", "a reply")
	if !strings.Contains(out, gheCommentURL) {
		t.Errorf("gh pr comment output = %q, want the comment url — the response was corrupted", out)
	}
	w := writes.only(t, "gh pr comment")
	if !w.ResponseUnread {
		t.Errorf("audit record = %+v, want the outcome marked unread for an over-cap response", w)
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
			env := ghEnvWithWrites(t, upstream.URL, writes.record)

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
	env := ghEnvWithWrites(t, upstream.URL, writes.record)

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
			env := ghEnvWithWrites(t, upstream.URL, writes.record)

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
