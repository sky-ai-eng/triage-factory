package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestClassifyGHCommand pins what the gate recognizes and — more importantly —
// what it leaves alone. The negative half is the invariant that matters day to
// day: a run that attempts neither action must see no evidence the gate
// exists, so every ordinary `gh`, `git` and `tfac` command has to fall
// straight through.
func TestClassifyGHCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    ghAction
	}{
		// --- merge ---
		{name: "the plain shape", command: "gh pr merge 7 --squash", want: ghActionMerge},
		{name: "no arguments at all", command: "gh pr merge", want: ghActionMerge},
		{name: "an absolute path", command: "/opt/tf/bin/gh pr merge 7", want: ghActionMerge},
		{name: "leading environment assignments", command: "GH_HOST=example.com GH_TOKEN=x gh pr merge 7", want: ghActionMerge},
		{name: "env(1)", command: "env GH_HOST=example.com gh pr merge 7", want: ghActionMerge},
		{name: "a flag between gh and its subcommand", command: "gh --repo owner/repo pr merge 7", want: ghActionMerge},
		{name: "a short flag between pr and merge", command: "gh pr -R owner/repo merge 7", want: ghActionMerge},
		{name: "an =-joined flag between pr and merge", command: "gh pr --repo=owner/repo merge 7", want: ghActionMerge},
		{name: "after a cd", command: "cd /work/repo && gh pr merge 7 --admin", want: ghActionMerge},
		{name: "on the far side of a pipe", command: "echo y | gh pr merge 7", want: ghActionMerge},
		{name: "a REST merge through gh api", command: "gh api -X PUT repos/owner/repo/pulls/7/merge", want: ghActionMerge},
		{name: "a REST merge with a leading slash", command: "gh api --method PUT /repos/owner/repo/pulls/7/merge -f merge_method=squash", want: ghActionMerge},
		{name: "a REST merge with a query string", command: "gh api repos/owner/repo/pulls/7/merge?per_page=1 -X PUT", want: ghActionMerge},

		// --- review ---
		{name: "an approving review", command: "gh pr review 7 --approve", want: ghActionReview},
		{name: "a commenting review", command: `gh pr review 7 --comment --body "looks good"`, want: ghActionReview},
		{name: "a review after a flag", command: "gh pr --repo owner/repo review 7 --request-changes -b nope", want: ghActionReview},

		// --- repo create ---
		{name: "the plain shape", command: "gh repo create acme/widgets --private", want: ghActionRepoCreate},
		{name: "no arguments at all", command: "gh repo create", want: ghActionRepoCreate},
		{name: "from a template", command: "gh repo create acme/widgets --template acme/tmpl", want: ghActionRepoCreate},
		{name: "a flag between repo and create", command: "gh repo --json name create acme/widgets", want: ghActionRepoCreate},
		{name: "after a cd", command: "cd /work && gh repo create acme/widgets", want: ghActionRepoCreate},
		{name: "the REST collection under a user", command: "gh api -X POST /user/repos -f name=widgets", want: ghActionRepoCreate},
		{name: "the REST collection under an org", command: "gh api --method POST orgs/acme/repos -f name=widgets", want: ghActionRepoCreate},
		{
			name:    "the mutation by hand",
			command: `gh api graphql -f query='mutation{createRepository(input:{name:"w",ownerId:"O_1"}){repository{url}}}'`,
			want:    ghActionRepoCreate,
		},
		{
			name:    "the template clone by hand",
			command: `gh api graphql -f query='mutation{cloneTemplateRepository(input:{name:"w"}){repository{url}}}'`,
			want:    ghActionRepoCreate,
		},
		{
			name:    "the =-joined field spelling",
			command: `gh api graphql --field=query='mutation{createRepository(input:{name:"w"}){repository{url}}}'`,
			want:    ghActionRepoCreate,
		},
		// gh switches off GET when a body file is supplied, with no method flag —
		// its own ruleset-create example relies on exactly that.
		{name: "a body file instead of fields", command: "gh api --input body.json orgs/acme/repos", want: ghActionRepoCreate},
		{name: "a body file, =-joined", command: "gh api --input=body.json /user/repos", want: ghActionRepoCreate},
		{name: "a body from stdin", command: "gh api --input - orgs/acme/repos", want: ghActionRepoCreate},
		// A repeated method flag resolves to the last, which is what gh's own
		// parsing does with it — so the read spelling in front of the write does
		// not hide the write.
		{name: "a method flag overridden to POST", command: "gh api -X GET /user/repos -X POST -f name=w", want: ghActionRepoCreate},
		// Instantiating a template makes a repository out of one that already
		// exists, so the path addresses the template and the act is still a create.
		{name: "the template endpoint", command: "gh api -X POST /repos/acme/tmpl/generate -f owner=acme -f name=w", want: ghActionRepoCreate},

		// --- negative space ---
		{name: "empty", command: ""},
		{name: "reading a PR", command: "gh pr view 7"},
		{name: "diffing a PR", command: "gh pr diff 7"},
		{name: "listing PRs", command: "gh pr list --state open"},
		{name: "creating a PR", command: `gh pr create --draft --title "fix" --body-file _tfac/pr-body.md`},
		{name: "marking a PR ready", command: "gh pr ready 7"},
		{name: "closing a PR", command: "gh pr close 7"},
		{name: "commenting on a PR", command: `gh pr comment 7 --body "pushed a fix"`},
		{name: "an issue verb", command: "gh issue view 12"},
		{name: "a run verb", command: "gh run view 993 --log-failed"},
		{name: "a read through gh api", command: "gh api repos/owner/repo/pulls/7"},
		{name: "the branch-merge endpoint is a different resource", command: "gh api -X POST repos/owner/repo/merges -f base=main -f head=topic"},
		{name: "merge-upstream is a different resource", command: "gh api -X POST repos/owner/repo/merge-upstream -f branch=main"},
		{name: "checking mergeability", command: "gh api repos/owner/repo/pulls/7 --jq .mergeable"},
		{name: "merge as a field value", command: "gh api repos/owner/repo/pulls/7 -f merge_method=merge"},
		{name: "the TF review verbs", command: "tfac gh pr start-review 7"},
		{name: "a local git merge", command: "git merge origin/main"},
		{name: "a git push", command: "git push -u origin aa/TFAC-1"},
		{name: "the word merge as a flag value", command: `gh pr comment 7 --body merge`},
		{name: "a quoted mention of the command", command: `echo "gh pr merge 7" >> _tfac/notes.md`},
		{name: "a grep for it", command: `grep -rn "gh pr review" .`},
		{name: "a lookalike binary", command: "ghost pr merge 7"},
		{name: "listing repos", command: "gh repo list acme"},
		{name: "viewing a repo", command: "gh repo view acme/widgets"},
		{name: "cloning a repo", command: "gh repo clone acme/widgets"},
		{name: "reading the repo collection", command: "gh api /user/repos"},
		{name: "reading an org's repo collection", command: "gh api orgs/acme/repos --paginate"},
		{name: "an explicit GET with fields is still a read", command: "gh api -X GET /user/repos -f per_page=100"},
		// The same rule in the direction that matters more day to day: a write
		// spelling overridden back to a read must not be refused.
		{name: "a method flag overridden to GET", command: "gh api -X POST /user/repos -X GET"},
		// The mutation names are matched only in the GraphQL query field. Ordinary
		// work whose TEXT mentions them is not repository creation — and in this
		// repository, prose containing those identifiers is entirely routine.
		{name: "a comment mentioning the mutation", command: `gh api repos/o/r/issues/1/comments -f body='we refuse createRepository now'`},
		{name: "a PR body mentioning the mutation", command: `gh api -X POST repos/o/r/pulls -f title=x -f body='maps cloneTemplateRepository to repo_created'`},
		{name: "a file named after it", command: "gh api --input createRepository.json repos/o/r/issues"},
		{name: "reading the template endpoint", command: "gh api /repos/acme/tmpl/generate"},
		{name: "a repo path that merely ends in a pair", command: "gh api -X POST repos/acme/repos -f x=y"},
		{name: "a repo-scoped path is not the collection", command: "gh api -X PATCH repos/acme/widgets -f description=x"},
		{name: "an org repo path is not the org collection", command: "gh api repos/acme/widgets/topics"},
		{name: "asking the schema about it", command: `gh api graphql -f query='query{__type(name:"CreateRepositoryInput"){name}}'`},
		{name: "a quoted mention of the command", command: `echo "gh repo create acme/widgets" >> _tfac/notes.md`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyGHCommand(tc.command); got != tc.want {
				t.Errorf("classifyGHCommand(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

// TestGHCommandGate_OnlyReadsBashCalls: the gate's whole surface is the shell.
// A tool whose arguments happen to carry the text must not be gated — the
// model writing "gh pr merge" into a file is reporting, not merging.
func TestGHCommandGate_OnlyReadsBashCalls(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-gate-bash", "", "/tmp/wt-gate-bash")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	gate := s.ghCommandGate(runmode.LocalDefaultOrgID, "r-gate-bash")

	write := domain.ToolCall{ID: "c1", Name: "write", Input: map[string]any{
		"file_path": "_tfac/notes.md", "content": "next step: gh pr merge 7",
	}}
	if deny := gate(context.Background(), write); deny != "" {
		t.Errorf("a non-bash call was gated: %q", deny)
	}
}

// TestAskedAboutMergeAlready pins when the merge question re-arms. Asking a
// model that has already restated its intent is obstruction; asking again
// after a human has changed the premise is a different question about
// different work.
//
// The state is read from the transcript rather than remembered, so an
// engagement that inherits a conversation behaves exactly like the one that
// asked — a crash cannot buy a second merge un-questioned, and cannot spend a
// question twice.
func TestAskedAboutMergeAlready(t *testing.T) {
	question := domain.Message{Role: "tool", ToolCallID: "c1", IsError: true, Content: mergeGateQuestion}
	assistant := domain.Message{Role: "assistant", Content: "my mission says to merge"}
	human := domain.Message{Role: "user", Content: "merge it when CI is green"}
	steered := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "go ahead and merge"}
	crashNotice := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionExecutorChanged, Content: "your executor changed"}
	stopNote := domain.Message{Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: "This run reached its spend cap and has been paused."}
	stagedNote := domain.Message{Role: "user", Subtype: "injection:system-note", Content: "<system-note>PR gained commits</system-note>"}
	echo := domain.Message{Role: "assistant", Content: "I was told: " + mergeGateQuestion}
	// The gate's tag is a string that exists in this repository and in every
	// transcript the note lands in, so tool output can carry it for reasons
	// that have nothing to do with a question having been put.
	grepHit := domain.Message{Role: "tool", ToolCallID: "c8", Content: "native_gh_gate.go:59:const mergeGateTag = `" + mergeGateTag + "`"}
	readTranscript := domain.Message{Role: "tool", ToolCallID: "c9", Content: `[{"role":"tool","content":"` + mergeGateQuestion + `"}]`}

	tests := []struct {
		name string
		rows []domain.Message
		want bool
	}{
		{
			name: "never asked",
			rows: []domain.Message{human, assistant},
		},
		{
			name: "asked, and nothing has happened since but the model's own answer",
			rows: []domain.Message{human, assistant, question, assistant},
			want: true,
		},
		{
			name: "a human spoke after the question, so the premise is new",
			rows: []domain.Message{human, assistant, question, assistant, human, assistant},
		},
		{
			// A follow-up that lands mid-work is stamped as a steer, and it is
			// still a person asking for more.
			name: "a mid-work steer counts as a human speaking",
			rows: []domain.Message{human, assistant, question, assistant, steered, assistant},
		},
		{
			name: "an executor-changed notice does not re-arm",
			rows: []domain.Message{human, assistant, question, crashNotice, assistant},
			want: true,
		},
		{
			name: "a stop-note from a guard park does not re-arm",
			rows: []domain.Message{human, assistant, question, assistant, stopNote, crashNotice, assistant},
			want: true,
		},
		{
			name: "a staged system note does not re-arm",
			rows: []domain.Message{human, assistant, question, stagedNote, assistant},
			want: true,
		},
		{
			// The reason the tag is looked for on the gate's own row: a model
			// that quotes the question back must not disarm a gate that has
			// never fired.
			name: "the model quoting the question is not the question",
			rows: []domain.Message{human, echo},
		},
		{
			// The reason the tag must OPEN the note rather than appear in it:
			// a run grepping this repository hands itself the string.
			name: "a grep hit on the gate's own source is not the question",
			rows: []domain.Message{human, assistant, grepHit, assistant},
		},
		{
			name: "reading a transcript that carries the note is not the question",
			rows: []domain.Message{human, assistant, readTranscript, assistant},
		},
		{
			name: "an empty transcript has nothing to have asked",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := askedAboutMergeAlready(tc.rows); got != tc.want {
				t.Errorf("askedAboutMergeAlready = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNativeGate_ReviewIsRefusedAndNeverDispatched drives the real engine over
// the real transcript with the real hook. `gh pr review` never reaches the
// jail, the refusal names the verbs that replace it, and it is given again the
// second time — the answer never changes and the redirect stays actionable, so
// repetition is not badgering. Neither denial ends the run.
func TestNativeGate_ReviewIsRefusedAndNeverDispatched(t *testing.T) {
	h := newGateHarness(t, "r-gate-review", "review the PR", []gateTurn{
		{calls: []domain.ToolCall{bashCall("c1", "gh pr review 7 --approve --body lgtm")}},
		{calls: []domain.ToolCall{bashCall("c2", "gh pr review 7 --comment -b nope")}},
		{text: "Using the review verbs instead."},
	})

	result := h.run()
	if result.Kind != agentloop.ResultConcluded {
		t.Fatalf("a refusal must not end the run: kind=%v err=%v", result.Kind, result.Err)
	}
	if got := h.host.commands(); len(got) != 0 {
		t.Fatalf("a refused review reached the jail: %q", got)
	}
	results := h.toolResults()
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want one refusal per attempt: %+v", len(results), results)
	}
	for i, r := range results {
		if !r.IsError || r.Content != reviewRefusal {
			t.Errorf("result %d = {is_error:%v, %q}, want the refusal in-band", i, r.IsError, r.Content)
		}
	}
	for _, verb := range []string{"tfac gh pr start-review", "add-review-comment", "finalize-review"} {
		if !strings.Contains(reviewRefusal, verb) {
			t.Errorf("the refusal does not redirect to %q", verb)
		}
	}
}

// TestNativeGate_MergeIsQuestionedThenAllowed walks the merge lifecycle: the
// first attempt is questioned and does not run, and the same call re-issued
// after the model has answered runs normally. The run concludes either way —
// the gate is a conversation, not a kill.
func TestNativeGate_MergeIsQuestionedThenAllowed(t *testing.T) {
	const merge = "gh pr merge 7 --squash --delete-branch"
	h := newGateHarness(t, "r-gate-merge", "land the fix", []gateTurn{
		{calls: []domain.ToolCall{bashCall("c1", merge)}},
		{text: `My mission says "land the fix".`, calls: []domain.ToolCall{bashCall("c2", merge)}},
		{text: "Merged."},
	})

	result := h.run()
	if result.Kind != agentloop.ResultConcluded {
		t.Fatalf("the merge gate must never fail a run: kind=%v err=%v", result.Kind, result.Err)
	}
	if got := h.host.commands(); len(got) != 1 || got[0] != merge {
		t.Fatalf("dispatched commands = %q, want only the re-issued merge", got)
	}
	results := h.toolResults()
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want the question then the real result: %+v", len(results), results)
	}
	if !results[0].IsError || !strings.Contains(results[0].Content, mergeGateTag) {
		t.Errorf("first result = {is_error:%v, %q}, want the merge question", results[0].IsError, results[0].Content)
	}
	if results[1].IsError {
		t.Errorf("the re-issued merge was denied a second time: %q", results[1].Content)
	}
	if !strings.Contains(results[0].Content, "the opening message of this conversation") {
		t.Error("the question must point at the mission, which is the opening message — not at an absent user")
	}
}

// TestNativeGate_MergeReArmsAfterHumanInput: a person speaking is a new
// premise, so the same question about that new work has not been put. The
// engagement below is a re-claim of a conversation that was already questioned
// and answered, with a follow-up waiting — the shape that must ask again.
func TestNativeGate_MergeReArmsAfterHumanInput(t *testing.T) {
	const merge = "gh pr merge 7"
	h := newGateHarness(t, "r-gate-rearm", "review the PR", []gateTurn{
		{text: "Done."},
	})

	// A prior stretch: the question was put, the model answered, and the merge
	// it re-issued went through.
	h.seed(domain.Message{Role: "assistant", Content: "merging"})
	h.seed(domain.Message{Role: "tool", ToolCallID: "c0", IsError: true, Content: mergeGateQuestion})
	h.seed(domain.Message{Role: "assistant", Content: "my mission says to merge"})

	gate := h.spawner.ghCommandGate(runmode.LocalDefaultOrgID, h.conversationID)
	if deny := gate(context.Background(), bashCall("c1", merge)); deny != "" {
		t.Fatalf("the question re-fired with no human input since: %q", deny)
	}

	h.seed(domain.Message{Role: "user", Content: "actually hold off, revert that"})
	if deny := gate(context.Background(), bashCall("c2", merge)); !strings.Contains(deny, mergeGateTag) {
		t.Fatalf("a human spoke and the question did not re-arm: %q", deny)
	}
}

// TestNativeGate_TextsMatchWhatTheInjectorDoes is the R5 check in test form,
// and it runs in both directions because this file makes two different kinds of
// promise.
//
// A text that says "refused" must correspond to a shape the injector actually
// refuses, or the model learns that this harness's rules are negotiable. And
// the merge text, which does NOT claim a refusal, must correspond to a shape
// the injector lets through — a question that says "quote your authorization
// and proceed" in front of a gate that would 403 the re-issue is the same lie
// told the other way round.
func TestNativeGate_TextsMatchWhatTheInjectorDoes(t *testing.T) {
	refusals := []struct {
		name    string
		refusal string
		req     ghwrite.Request
	}{
		{"review", reviewRefusal, ghwrite.Request{Method: http.MethodPost, Path: "/repos/acme/widgets/pulls/7/reviews"}},
		{"repo create", repoCreateRefusal, ghwrite.Request{Method: http.MethodPost, Path: "/user/repos"}},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			if _, gated := ghwrite.Gate(tc.req); !gated {
				t.Fatalf("the matcher refuses %s but the injector forwards it", tc.name)
			}
			if !strings.Contains(tc.refusal, "This command was not run") {
				t.Errorf("refusal does not say the command did not run: %q", tc.refusal)
			}
			if !strings.Contains(tc.refusal, "refused too") {
				t.Errorf("refusal does not say another spelling fares no better: %q", tc.refusal)
			}
		})
	}

	// The merge question's whole shape is "answer this and you may proceed", so
	// the merge must reach GitHub once the model re-issues.
	merges := []ghwrite.Request{
		{Method: http.MethodPut, Path: "/repos/acme/widgets/pulls/7/merge"},
		{Method: http.MethodPost, Path: "/api/graphql",
			GraphQL: &ghwrite.GraphQLFacts{Fields: []string{"mergePullRequest"}}},
		{Method: http.MethodPost, Path: "/api/graphql",
			GraphQL: &ghwrite.GraphQLFacts{Fields: []string{"enablePullRequestAutoMerge"}}},
	}
	for _, req := range merges {
		if ref, gated := ghwrite.Gate(req); gated {
			t.Errorf("the merge question promises a re-issue proceeds, but the injector refuses %s %s as %q",
				req.Method, req.Path, ref.Reason)
		}
	}
	if strings.Contains(mergeGateQuestion, "refused") {
		t.Error("the merge question must not claim a refusal it does not deliver")
	}

	// The review redirect names commands that have to exist, since a redirect
	// to a verb nobody can run is worse than no redirect at all.
	for _, verb := range []string{"tfac gh pr start-review", "add-review-comment", "finalize-review"} {
		if !strings.Contains(reviewRefusal, verb) {
			t.Errorf("the review refusal does not redirect to %q", verb)
		}
	}
}

// --- harness ---
//
// A real engine over the real nativeTranscript, with the provider and the jail
// scripted. It is what lets "the call never reached the jail" be asserted as an
// observation rather than inferred from the hook's return value.

type gateTurn struct {
	text  string
	calls []domain.ToolCall
}

type gateHarness struct {
	t              *testing.T
	spawner        *Spawner
	conversationID string
	claimID        string
	host           *recordingToolHost
	turns          []gateTurn
}

func newGateHarness(t *testing.T, conversationID, mission string, turns []gateTurn) *gateHarness {
	t.Helper()
	database := newDelegateTestDB(t)
	seedConversation(t, database, conversationID, "", "/tmp/wt-"+conversationID)
	claimID := markEngaged(t, database, conversationID)
	markNative(t, database, conversationID)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-5")

	h := &gateHarness{t: t, spawner: s, conversationID: conversationID, claimID: claimID, host: &recordingToolHost{}, turns: turns}
	if _, err := newNativeTranscript(s, runmode.LocalDefaultOrgID, conversationID, claimID).
		Insert(context.Background(), runmode.LocalDefaultOrgID, pendingUserInput(conversationID, runmode.LocalDefaultUserID, mission)); err != nil {
		t.Fatalf("seed the mission: %v", err)
	}
	return h
}

// seed appends a row directly, staging transcript history the scripted turns
// do not produce.
func (h *gateHarness) seed(row domain.Message) {
	h.t.Helper()
	row.ConversationID = h.conversationID
	if _, err := h.spawner.conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &row); err != nil {
		h.t.Fatalf("seed row: %v", err)
	}
}

func (h *gateHarness) run() agentloop.Result {
	h.t.Helper()
	engine := &agentloop.Engine{
		Transcript:  newNativeTranscript(h.spawner, runmode.LocalDefaultOrgID, h.conversationID, h.claimID),
		Credentials: staticGateCredentials{provider: &scriptedGateProvider{turns: h.turns}},
		Tools:       h.host,
		Hooks: agentloop.Hooks{
			BeforeToolCall: h.spawner.ghCommandGate(runmode.LocalDefaultOrgID, h.conversationID),
		},
	}
	return engine.Run(context.Background(), agentloop.Params{
		OrgID:          runmode.LocalDefaultOrgID,
		ConversationID: h.conversationID,
		Model:          "claude-sonnet-4-5",
		SystemPrompt:   "system",
		HasBlueprint:   true,
	})
}

// toolResults returns the conversation's tool rows in assembly order.
func (h *gateHarness) toolResults() []domain.Message {
	h.t.Helper()
	rows, err := h.spawner.conversations.ListForAssemblySystem(context.Background(), runmode.LocalDefaultOrgID, h.conversationID)
	if err != nil {
		h.t.Fatalf("list for assembly: %v", err)
	}
	var out []domain.Message
	for _, r := range rows {
		if r.Role == "tool" {
			out = append(out, r)
		}
	}
	return out
}

func bashCall(id, command string) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: "bash", Input: map[string]any{"command": command}}
}

// recordingToolHost answers every call and records the shell command it was
// handed, so a denial's absence from this list is the assertion.
type recordingToolHost struct{ observed []string }

func (h *recordingToolHost) Call(name string, args map[string]any) (agentloop.ToolOutcome, error) {
	cmd, _ := args["command"].(string)
	h.observed = append(h.observed, cmd)
	return agentloop.ToolOutcome{Content: "ok:" + name}, nil
}

func (h *recordingToolHost) Close() error { return nil }

func (h *recordingToolHost) commands() []string { return append([]string(nil), h.observed...) }

// scriptedGateProvider replays fixed assistant turns; running past the end is a
// test bug and surfaces as an error rather than a hang.
type scriptedGateProvider struct {
	turns []gateTurn
	calls int
}

func (p *scriptedGateProvider) Stream(_ context.Context, _ inference.Request) (*inference.Completion, error) {
	if p.calls >= len(p.turns) {
		return nil, fmt.Errorf("scripted provider ran out of turns after %d calls", p.calls)
	}
	turn := p.turns[p.calls]
	p.calls++

	msg := schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant}
	if turn.text != "" {
		text := turn.text
		msg.Content = &schemas.ChatMessageContent{ContentStr: &text}
	}
	finish := "stop"
	if len(turn.calls) > 0 {
		finish = "tool_calls"
		toolCalls := make([]schemas.ChatAssistantMessageToolCall, len(turn.calls))
		for i, c := range turn.calls {
			id, name := c.ID, c.Name
			args := "{}"
			if len(c.Input) > 0 {
				raw, err := json.Marshal(c.Input)
				if err != nil {
					return nil, err
				}
				args = string(raw)
			}
			fn := "function"
			toolCalls[i] = schemas.ChatAssistantMessageToolCall{
				Index:    uint16(i),
				Type:     &fn,
				ID:       &id,
				Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args},
			}
		}
		msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{ToolCalls: toolCalls}
	}
	return &inference.Completion{Message: msg, FinishReason: finish}, nil
}

type staticGateCredentials struct{ provider agentloop.Provider }

func (c staticGateCredentials) ForCall(context.Context) (schemas.ModelProvider, agentloop.Provider, func(), error) {
	return inference.ProviderAnthropic, c.provider, nil, nil
}

// TestNativeGate_RepoCreateIsRefusedAndNeverDispatched: repository creation is
// refused outright rather than questioned, because unlike a merge there is no
// mission the runtime could be given that makes it right. The refusal names no
// replacement verb — none exists — so this pins that it at least tells the
// model what to do instead, which is stop and say so.
func TestNativeGate_RepoCreateIsRefusedAndNeverDispatched(t *testing.T) {
	h := newGateHarness(t, "r-gate-repo-create", "set up the new service", []gateTurn{
		{calls: []domain.ToolCall{bashCall("c1", "gh repo create acme/widgets --private")}},
		{calls: []domain.ToolCall{bashCall("c2", `gh api graphql -f query='mutation{createRepository(input:{name:"widgets",ownerId:"O_1"}){repository{url}}}'`)}},
		{text: "Cannot create the repository; reporting instead."},
	})

	result := h.run()
	if result.Kind != agentloop.ResultConcluded {
		t.Fatalf("a refusal must not end the run: kind=%v err=%v", result.Kind, result.Err)
	}
	if got := h.host.commands(); len(got) != 0 {
		t.Fatalf("a refused repo create reached the jail: %q", got)
	}
	results := h.toolResults()
	if len(results) != 2 {
		t.Fatalf("tool results = %d, want one refusal per attempt: %+v", len(results), results)
	}
	for i, r := range results {
		if !r.IsError || r.Content != repoCreateRefusal {
			t.Errorf("result %d = {is_error:%v, %q}, want the refusal in-band", i, r.IsError, r.Content)
		}
	}
	// No verb is named because none exists, and the file's own rule is that a
	// redirect to a command that does not exist is worse than none. What the
	// text must still carry is the actionable alternative.
	if strings.Contains(repoCreateRefusal, "tfac ") {
		t.Error("the refusal names a tfac verb; there is no repo verb to name")
	}
	if !strings.Contains(repoCreateRefusal, "final message") {
		t.Error("the refusal does not tell the model what to do instead")
	}
}
