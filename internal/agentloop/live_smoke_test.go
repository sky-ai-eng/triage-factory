package agentloop

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// TestNativeLoop_LiveSmoke drives the whole engine against a real provider
// and the real compiled tool host: assemble → stream → persist → dispatch
// into the host → conclude. It is the loop half of the production path; the
// jail half (cap-broker launch, per-run network, gh injector) needs a runsc
// host and is exercised by the acceptance run, not by `go test`.
//
// Skipped by default. Opt in with:
//
//	TF_TEST_LOOP_LIVE=1 ANTHROPIC_API_KEY=sk-... \
//	  go test ./internal/agentloop -run TestNativeLoop_LiveSmoke -v
//
// Optional: TF_TEST_LOOP_MODEL to pick the model. Bills real money.
func TestNativeLoop_LiveSmoke(t *testing.T) {
	if os.Getenv("TF_TEST_LOOP_LIVE") != "1" {
		t.Skip("set TF_TEST_LOOP_LIVE=1 to run the live native-loop smoke test")
	}
	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY is required for the live native-loop smoke test")
	}
	bin := harnessBinary(t)
	if bin == "" {
		t.Skip("tf-harness-tools is not built; run `cargo build` in harness/tf-harness-tools")
	}

	model := strings.TrimSpace(os.Getenv("TF_TEST_LOOP_MODEL"))
	if model == "" {
		model = "claude-sonnet-4-5"
	}

	// A worktree with one file the model has to actually read to answer.
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "SECRET.txt"), []byte("the answer is plum\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Bind here and let the host dial in — the production direction, since
	// the jail cannot create a host socket under --host-uds=open.
	sock := filepath.Join(t.TempDir(), "tools.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("bind tool host socket: %v", err)
	}
	defer listener.Close()

	conn := startAndAccept(t, listener, exec.Command(bin, "serve", "--connect", sock, "--cwd", work))
	host := NewToolHost(conn, 2*time.Minute)
	defer host.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Production shape: the mission rides the opening turn, not the system
	// prompt, so the seeded row is the whole of what this run is asked to do.
	tr := newMemTranscript(pendingUser("Answer the user's question using your tools.\n\n" +
		"Read SECRET.txt in the working directory and tell me the answer it names. Then stop."))
	engine := &Engine{
		Transcript: tr,
		Credentials: &EnvCredentials{
			Resolve: func(context.Context) (map[string]string, error) {
				return map[string]string{"ANTHROPIC_API_KEY": apiKey}, nil
			},
			Models: []string{model},
		},
		Tools: host,
	}

	got := engine.Run(ctx, Params{
		OrgID:          "live",
		ConversationID: "live-conv",
		Model:          model,
		// The real composed prompt rather than a stub: this is the one test
		// that runs a live model against the real tool host, so it is also the
		// only place the production prompt and the production tool schemas are
		// put in front of a provider together.
		SystemPrompt: agentprompt.Build(agentprompt.Spec{
			Surface: agentprompt.SurfaceMachinist,
			Runtime: agentprompt.RuntimeNative,
			Family:  agentprompt.FamilyClaude,
			Mode:    agentprompt.ModeMulti,
		}),
	})

	if got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	if !strings.Contains(strings.ToLower(got.ResultSummary), "plum") {
		t.Errorf("the model must have read the file; summary = %q", got.ResultSummary)
	}

	// The transcript must be legal and priced: every tool call answered, and
	// every assistant row carrying usage and a cost stamp.
	answered := map[string]bool{}
	for _, r := range tr.snapshot() {
		if r.Role == "tool" {
			answered[r.ToolCallID] = true
		}
	}
	var assistants int
	for _, r := range tr.snapshot() {
		if r.Role != "assistant" {
			continue
		}
		assistants++
		if r.InputTokens == nil || r.OutputTokens == nil {
			t.Errorf("assistant row %d has no usage: %+v", r.ID, r)
		}
		if r.CostUSD == nil {
			t.Errorf("assistant row %d has no cost stamp for a known model", r.ID)
		}
		for _, call := range r.ToolCalls {
			if !answered[call.ID] {
				t.Errorf("tool call %s (%s) has no result row", call.ID, call.Name)
			}
		}
	}
	if assistants == 0 {
		t.Fatal("no assistant rows were persisted")
	}
	t.Logf("live loop: %d turns, %d assistant rows, summary %q", got.NumTurns, assistants, got.ResultSummary)
}

// TestLiveAssemblyRoundTrip is a cheap guard that the rows the loop writes
// re-assemble into a legal request — the property a resumed engagement
// depends on. It needs no provider.
func TestLiveAssemblyRoundTrip(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "go"},
		{ID: 2, Role: "assistant", Content: "reading",
			ToolCalls: []domain.ToolCall{{ID: "t1", Name: "read", Input: map[string]any{"path": "x"}}}},
		{ID: 3, Role: "tool", ToolCallID: "t1", Content: "contents"},
		{ID: 4, Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "also check y"},
	}
	msgs, err := inference.RowsToMessages(rows, inference.AssemblyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[1].Role != schemas.ChatMessageRoleAssistant || len(msgs[1].ToolCalls) != 1 {
		t.Errorf("the assistant turn must carry its tool call: %+v", msgs[1])
	}
	if msgs[2].Role != schemas.ChatMessageRoleTool || msgs[2].ToolCallID == nil || *msgs[2].ToolCallID != "t1" {
		t.Errorf("the tool result must stay linked to its call: %+v", msgs[2])
	}
}
