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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cabundle"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghwrite"
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
	// The review comment a thread reply creates, as GitHub links it.
	gheReplyNodeID = "PRRC_kwWireReply"
	gheReplyURL    = "https://ghe.test/octo/repo/pull/42#discussion_r8"
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

		case strings.Contains(query, "addPullRequestReviewThreadReply"):
			_, _ = w.Write([]byte(`{"data":{"addPullRequestReviewThreadReply":{"comment":{` +
				`"id":"` + gheReplyNodeID + `","databaseId":8,"url":"` + gheReplyURL + `"}}}}`))

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
	return ghEnvWithConfig(t, upstream, Config{ObserveWrite: observeWrite, AuthorizeWrite: authorize})
}

// ghEnvWithReads is the harness with the read touch wired and nothing else: a
// read command needs no audit and reaches no gate.
func ghEnvWithReads(t testing.TB, upstream string, observeRead func(context.Context, ObservedRead)) []string {
	return ghEnvWithConfig(t, upstream, Config{ObserveRead: observeRead})
}

// ghEnvWithConfig stands the injector up with the callbacks callbacks carries;
// the upstream, placeholder, certificate and credential are the harness's own.
func ghEnvWithConfig(t testing.TB, upstream string, callbacks Config) []string {
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
		ObserveWrite:   callbacks.ObserveWrite,
		ObserveRead:    callbacks.ObserveRead,
		AuthorizeWrite: callbacks.AuthorizeWrite,
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
	if err := os.WriteFile(trust, cabundle.WithSystemRoots(certPEM), 0o600); err != nil {
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
		name       string
		args       []string
		mutation   string
		action     string
		target     string
		externalID string
		nodeID     string
		url        string
	}{
		{
			name:     "pr comment",
			args:     []string{"pr", "comment", "42", "-R", ghePROwner + "/" + ghePRRepo, "--body", "a reply"},
			mutation: "addComment",
			action:   domain.ActionCommentPosted,
			// The request named only a node id; the response located it, and
			// the anchor on that url is the comment's own number. gh does not
			// select the comment's node id, so the row carries none.
			target:     ghePROwner + "/" + ghePRRepo + "#" + strconv.Itoa(ghePRNumber),
			externalID: "7",
			url:        gheCommentURL,
		},
		{
			name:     "pr close",
			args:     []string{"pr", "close", "42", "-R", ghePROwner + "/" + ghePRRepo},
			mutation: "closePullRequest",
			action:   domain.ActionPRClosed,
			// gh selects only the id back, so the node id is all this row can
			// honestly carry — as its target and its node id, never as an
			// external id, which for a pull request is its number.
			target: ghePRNodeID,
			nodeID: ghePRNodeID,
		},
		{
			name:     "pr merge",
			args:     []string{"pr", "merge", "42", "-R", ghePROwner + "/" + ghePRRepo, "--merge"},
			mutation: "mergePullRequest",
			action:   domain.ActionPRMerged,
			target:   ghePRNodeID,
			nodeID:   ghePRNodeID,
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
			if shape.ExternalID != tc.externalID {
				t.Errorf("external id = %q, want %q", shape.ExternalID, tc.externalID)
			}
			if shape.NodeID != tc.nodeID {
				t.Errorf("node id = %q, want %q", shape.NodeID, tc.nodeID)
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

// TestGHWire_ThreadReplyIsKeyedOnTheReply: no porcelain command replies to a
// review thread through GraphQL, so this is the hand-written spelling, with the
// input passed as variables the way `gh api` builds them. Its row is keyed on
// the reply it created — the number from the response and the node id a later
// GraphQL edit or delete would name it by.
func TestGHWire_ThreadReplyIsKeyedOnTheReply(t *testing.T) {
	upstream, _ := fakeGHE(t, "")
	var writes writeObservations
	env := ghEnvWithWrites(t, upstream.URL, writes.record)

	runGH(t, env, "api", "graphql",
		"-f", `query=mutation($input:AddPullRequestReviewThreadReplyInput!){addPullRequestReviewThreadReply(input:$input){comment{id databaseId url}}}`,
		"-f", "input[pullRequestReviewThreadId]=PRRT_kwThread", "-f", "input[body]=ack")

	w := writes.only(t, "gh api graphql")
	if w.NodeID != gheReplyNodeID {
		t.Errorf("node id = %q, want the reply's own %q", w.NodeID, gheReplyNodeID)
	}
	shape, ok := ghwrite.Resolve(w)
	if !ok || !w.Succeeded() || shape.Action != domain.ActionCommentPosted {
		t.Fatalf("record = %+v resolved to %+v (ok=%v), want comment_posted", w, shape, ok)
	}
	if shape.ExternalID != "8" {
		t.Errorf("external id = %q, want the reply's number 8", shape.ExternalID)
	}
	if want := ghePROwner + "/" + ghePRRepo + "#" + strconv.Itoa(ghePRNumber); shape.Target() != want {
		t.Errorf("target = %q, want %q from the reply's url", shape.Target(), want)
	}
}

// TestGHWire_AliasedReplyKeepsItsActAndLosesItsIDs: a selection that aliases a
// field onto a name the response read relies on gets back that field's value
// under that name — here, as GitHub would answer `databaseId: body` — so the row
// takes nothing from the response. The act is still named and still located by
// the thread its request named.
func TestGHWire_AliasedReplyKeepsItsActAndLosesItsIDs(t *testing.T) {
	upstream, _ := fakeGHE(t, "")
	var writes writeObservations
	env := ghEnvWithWrites(t, upstream.URL, writes.record)

	runGH(t, env, "api", "graphql",
		"-f", `query=mutation($input:AddPullRequestReviewThreadReplyInput!){addPullRequestReviewThreadReply(input:$input){comment{id databaseId: body url}}}`,
		"-f", "input[pullRequestReviewThreadId]=PRRT_kwThread", "-f", "input[body]=8")

	w := writes.only(t, "gh api graphql")
	if w.ExternalID != "" || w.NodeID != "" || w.URL != "" {
		t.Errorf("record = %+v, want no id, node id, or url from an aliased response", w)
	}
	shape, ok := ghwrite.Resolve(w)
	if !ok || !w.Succeeded() || shape.Action != domain.ActionCommentPosted {
		t.Fatalf("record = %+v resolved to %+v (ok=%v), want comment_posted", w, shape, ok)
	}
	if shape.ExternalID != "" || shape.Target() != "PRRT_kwThread" {
		t.Errorf("shape = %+v, want no id and the thread's node id as the target", shape)
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

// readObservations collects what the read touch reported.
type readObservations struct {
	mu   sync.Mutex
	seen []ObservedRead
}

func (o *readObservations) record(_ context.Context, r ObservedRead) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, r)
}

func (o *readObservations) snap() []ObservedRead {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ObservedRead(nil), o.seen...)
}

// fakeGHEReads answers what gh's read commands ask the conformance fixture's
// repository for: its GraphQL finders (graphQLFixture), and the REST reads of
// pull request 42 and issue 7 that `gh pr diff`, `gh api` and the injector's
// own identity reads make. It answers the injector's lookup of a branch's open
// pull requests too: pull request 42 is open on `feature`, two are open on
// `two-prs`, and none on any other branch. gh's own lookup of a branch is
// answered with no pull request whatever the branch, so a touch that follows
// one can only have come from the injector's lookup. It returns the server,
// the number of times the pull request's own REST object was read, and the
// number of times the injector looked up a branch.
func fakeGHEReads(t *testing.T) (srv *httptest.Server, pullReads func() int, branchLookups func(branch string) int) {
	t.Helper()
	const repoJSON = `{"id":4,"full_name":"octo/repo"}`
	var (
		mu      sync.Mutex
		pulls   int
		lookups = map[string]int{}
	)
	openPull := func(number int, nodeID, branch string) string {
		return `{"number":` + strconv.Itoa(number) + `,"node_id":"` + nodeID + `","head":{"ref":"` + branch + `"},"base":{"repo":` + repoJSON + `}}`
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, restPrefix)
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			var envelope struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(body, &envelope)
			switch {
			case strings.Contains(envelope.Query, "PullRequestForBranch"), strings.Contains(envelope.Query, "PullRequestList"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"totalCount":0,"nodes":[],` +
					`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`))
			case strings.Contains(envelope.Query, "PullRequestStatus"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"defaultBranchRef":{"name":"main"},` +
					`"pullRequests":{"totalCount":0,"edges":[]}},` +
					`"viewerCreated":{"totalCount":0,"edges":[]},"reviewRequested":{"totalCount":0,"edges":[]}}}`))
			case strings.Contains(envelope.Query, "UserCurrent"):
				_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"agent"}}}`))
			case strings.HasPrefix(envelope.Query, "{"), strings.HasPrefix(envelope.Query, "query("):
				// A hand-written query: gh names every operation it sends.
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"title":"T"}}}}`))
			default:
				status, response := graphQLFixture(envelope.Query, body)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(response))
			}
		case path == "/meta":
			_, _ = w.Write([]byte(`{"verifiable_password_authentication":false}`))
		case path == "/repos/octo/repo/pulls" && r.URL.Query().Has("head"):
			branch := strings.TrimPrefix(r.URL.Query().Get("head"), "octo:")
			mu.Lock()
			lookups[branch]++
			mu.Unlock()
			switch {
			case r.URL.Query().Get("state") != "open":
				w.WriteHeader(http.StatusBadRequest)
			case branch == "feature":
				_, _ = w.Write([]byte(`[` + openPull(42, "PR_open", branch) + `]`))
			case branch == "two-prs":
				_, _ = w.Write([]byte(`[` + openPull(43, "PR_one", branch) + `,` + openPull(44, "PR_two", branch) + `]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		case path == "/repos/octo/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case path == "/repos/octo/repo/pulls/42":
			mu.Lock()
			pulls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"number":42,"node_id":"PR_open","base":{"repo":` + repoJSON + `}}`))
		case path == "/repos/octo/repo/issues/7":
			_, _ = w.Write([]byte(`{"number":7,"node_id":"I_open"}`))
		case path == "/repos/octo/repo/issues/7/comments":
			_, _ = w.Write([]byte(`[]`))
		case path == "/repos/octo/repo":
			_, _ = w.Write([]byte(repoJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
			mu.Lock()
			defer mu.Unlock()
			return pulls
		}, func(branch string) int {
			mu.Lock()
			defer mu.Unlock()
			return lookups[branch]
		}
}

// TestGHWire_AddressedReadsAreTouched drives the pinned gh's read commands and
// asserts what the read touch reports: the pull request or issue a command
// addresses by number, once, with GitHub's ids for it, whichever transport the
// command used; the one pull request open on a branch a command looks up; and
// nothing for a command that reads a set, or looks up a branch with no pull
// request or several open on it. The documents gh sends are the contract here,
// as they are for the write audit: the reader was written against them, so the
// binary is what has to drive it.
func TestGHWire_AddressedReadsAreTouched(t *testing.T) {
	repo := []string{"-R", ghePROwner + "/" + ghePRRepo}
	pull := ObservedRead{
		ObjectRef: ghwrite.ObjectRef{Owner: "octo", Repo: "repo", Number: 42},
		Identity:  domain.GitHubIdentity{RepositoryID: "4", Repository: "octo/repo", NodeID: "PR_open", Kind: domain.EntityKindPullRequest},
	}
	issue := ObservedRead{
		ObjectRef: ghwrite.ObjectRef{Owner: "octo", Repo: "repo", Number: 7},
		Identity:  domain.GitHubIdentity{RepositoryID: "4", Repository: "octo/repo", NodeID: "I_open", Kind: domain.EntityKindIssue},
	}
	cases := []struct {
		args []string
		want []ObservedRead
	}{
		{args: append([]string{"pr", "view", "42"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"pr", "view", "42", "--json", "title,files"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"pr", "view", "42", "--comments"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"pr", "diff", "42"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"issue", "view", "7"}, repo...), want: []ObservedRead{issue}},
		{args: []string{"api", "repos/octo/repo/pulls/42"}, want: []ObservedRead{pull}},
		{args: []string{"api", "repos/octo/repo/issues/7/comments"}, want: []ObservedRead{issue}},
		{args: []string{"api", "graphql", "-f",
			`query={ repository(owner:"octo", name:"repo") { pullRequest(number: 42) { title } } }`}, want: []ObservedRead{pull}},
		{args: []string{"api", "graphql", "-F", "owner=octo", "-F", "name=repo", "-F", "number=42", "-f",
			`query=query($owner:String!,$name:String!,$number:Int!){ repository(owner:$owner, name:$name) { pullRequest(number:$number) { title } } }`},
			want: []ObservedRead{pull}},

		// A lookup by branch name, the one gh makes for a command given a
		// branch or no number at all. The rule follows the request, so a list
		// filtered to one branch sends the same lookup.
		{args: append([]string{"pr", "view", "feature"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"pr", "checks", "feature"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"pr", "list", "--head", "feature"}, repo...), want: []ObservedRead{pull}},
		{args: append([]string{"pr", "view", "some-branch"}, repo...)},
		{args: append([]string{"pr", "view", "two-prs"}, repo...)},
		// With --repo, gh reads no current branch and sends the lookup empty.
		{args: append([]string{"pr", "status"}, repo...)},

		// Sets.
		{args: append([]string{"pr", "list"}, repo...)},
		{args: []string{"api", "repos/octo/repo/pulls"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			upstream, _, _ := fakeGHEReads(t)
			var reads readObservations
			env := ghEnvWithReads(t, upstream.URL, reads.record)

			// A command's exit status is beside the point (a list against an
			// empty fixture, a branch with no pull request): what it addressed
			// is what is asserted.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, ghBinary(t), tc.args...)
			cmd.Env = env
			cmd.Dir = t.TempDir()
			out, _ := cmd.CombinedOutput()

			if got := reads.snap(); !slices.Equal(got, tc.want) {
				t.Errorf("read touches = %+v, want %+v\ngh output:\n%s", got, tc.want, out)
			}
		})
	}
}

// TestGHWire_ARunReadsAnObjectsIdentityOnce: every read of a pull request
// after the first costs nothing of the injector's own, across commands and
// transports.
func TestGHWire_ARunReadsAnObjectsIdentityOnce(t *testing.T) {
	upstream, pullReads, _ := fakeGHEReads(t)
	var reads readObservations
	env := ghEnvWithReads(t, upstream.URL, reads.record)
	repo := []string{"-R", ghePROwner + "/" + ghePRRepo}

	runGH(t, env, append([]string{"pr", "view", "42"}, repo...)...)
	runGH(t, env, append([]string{"pr", "view", "42", "--json", "title"}, repo...)...)
	runGH(t, env, "api", "graphql", "-f", `query={ repository(owner:"OCTO", name:"Repo") { pullRequest(number: 42) { title } } }`)

	if got := reads.snap(); len(got) != 1 {
		t.Errorf("read touches = %+v, want the pull request reported once", got)
	}
	// gh's own commands above are GraphQL, so every REST read of the pull
	// request is the injector's.
	if n := pullReads(); n != 1 {
		t.Errorf("identity reads of the pull request = %d, want 1", n)
	}
}

// checkoutOnBranch makes a git repository checked out on branch, with one
// commit, for a gh command that reads the current branch. It runs git with no
// configuration of the host's, as the jail does.
func checkoutOnBranch(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not installed: %v", err)
	}
	dir := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=wire", "GIT_AUTHOR_EMAIL=wire@example.test",
		"GIT_COMMITTER_NAME=wire", "GIT_COMMITTER_EMAIL=wire@example.test",
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=" + branch},
		{"commit", "--quiet", "--allow-empty", "--message", "wire"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// TestGHWire_NoNumberTouchesTheBranchsPullRequest is the branch lookup as an
// agent meets it: `gh pr view`, `gh pr status` and `gh pr checks` with no
// number, run in a checkout of a branch, touch the one pull request open on
// that branch, and nothing when no pull request or several are open on it.
//
// gh finds the repository through GH_REPO here, because it matches a remote
// against GH_HOST with the port stripped, and a test cannot serve on 443. The
// current branch is read from the checkout as it is in the jail.
func TestGHWire_NoNumberTouchesTheBranchsPullRequest(t *testing.T) {
	pull := ObservedRead{
		ObjectRef: ghwrite.ObjectRef{Owner: "octo", Repo: "repo", Number: 42},
		Identity:  domain.GitHubIdentity{RepositoryID: "4", Repository: "octo/repo", NodeID: "PR_open", Kind: domain.EntityKindPullRequest},
	}
	for _, branch := range []string{"feature", "some-branch", "two-prs"} {
		var want []ObservedRead
		if branch == "feature" {
			want = []ObservedRead{pull}
		}
		dir := checkoutOnBranch(t, branch)
		for _, command := range []string{"view", "status", "checks"} {
			t.Run(command+" on "+branch, func(t *testing.T) {
				upstream, _, lookups := fakeGHEReads(t)
				var reads readObservations
				env := append(ghEnvWithReads(t, upstream.URL, reads.record), "GH_REPO="+ghePROwner+"/"+ghePRRepo)

				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, ghBinary(t), "pr", command)
				cmd.Env = env
				cmd.Dir = dir
				out, _ := cmd.CombinedOutput()

				if got := reads.snap(); !slices.Equal(got, want) {
					t.Errorf("read touches = %+v, want %+v\ngh output:\n%s", got, want, out)
				}
				if n := lookups(branch); n != 1 {
					t.Errorf("lookups of %q = %d, want 1\ngh output:\n%s", branch, n, out)
				}
			})
		}
	}
}

// TestGHWire_ARunLooksABranchUpOnce: a branch is looked up once per run
// whatever the lookup answered, and the pull request it found is the one
// object the run has read, however it reaches it after that.
func TestGHWire_ARunLooksABranchUpOnce(t *testing.T) {
	upstream, pullReads, lookups := fakeGHEReads(t)
	var reads readObservations
	env := ghEnvWithReads(t, upstream.URL, reads.record)
	repo := []string{"-R", ghePROwner + "/" + ghePRRepo}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, ghBinary(t), append(args, repo...)...)
		cmd.Env = env
		cmd.Dir = t.TempDir()
		_, _ = cmd.CombinedOutput()
	}

	run("pr", "view", "feature")
	run("pr", "checks", "feature")
	run("pr", "view", "feature", "--json", "title")
	run("pr", "view", "42")
	run("pr", "view", "some-branch")
	run("pr", "view", "some-branch")

	if got := reads.snap(); len(got) != 1 || got[0].Number != 42 {
		t.Errorf("read touches = %+v, want pull request 42 reported once", got)
	}
	if n := lookups("feature"); n != 1 {
		t.Errorf("lookups of feature = %d, want 1", n)
	}
	if n := lookups("some-branch"); n != 1 {
		t.Errorf("lookups of some-branch = %d, want 1: an answer of none is remembered too", n)
	}
	// The lookup answered what GitHub calls the pull request, so the read of it
	// by number costs no read of the injector's own.
	if n := pullReads(); n != 0 {
		t.Errorf("identity reads of the pull request = %d, want 0", n)
	}
}
