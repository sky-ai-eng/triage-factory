package agentproc_test

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"os"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestPlugins_LiveDenyPluginRefusesTheProbeCommand runs a real Claude Code
// session with the denyplugin fixture as a required plugin, asks the model to
// run a Bash command naming tf-plugin-probe-deny, and checks that the call
// comes back as an is_error tool result carrying the plugin's reason. Bash is
// on the allowlist, so the plugin's hook is the only thing that refuses it.
// It runs once unsandboxed and, on Linux with bubblewrap, once under the
// local sandbox, where the plugin directory is bound read-only and the home
// directory is masked.
//
// Gated like the other live SDK tests, and billed the same way:
//
//	TF_TEST_SDK_LIVE=1 go test ./internal/agentproc -run TestPlugins_Live -v
func TestPlugins_LiveDenyPluginRefusesTheProbeCommand(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK plugin test")
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "denyplugin"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unsandboxed", func(t *testing.T) {
		runDenyPluginProbe(t, fixture, false)
	})
	t.Run("bubblewrap", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("the local sandbox exists only on Linux")
		}
		if err := localsandbox.Probe(); err != nil {
			t.Skipf("bubblewrap cannot build a namespace here: %v", err)
		}
		runDenyPluginProbe(t, fixture, true)
	})
}

func runDenyPluginProbe(t *testing.T, fixture string, sandboxed bool) {
	t.Helper()
	runRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plugins, warnings, err := agentproc.PreparePlugins([]domain.LocalPlugin{{Path: fixture, Required: true}},
		agentproc.PluginPrepareContext{RunRoot: runRoot, RunsBase: worktree.RunsBase()})
	if err != nil || len(warnings) > 0 {
		t.Fatalf("PreparePlugins: err=%v warnings=%v", err, warnings)
	}

	var spec *localsandbox.Spec
	if sandboxed {
		projectDir, err := worktree.ClaudeProjectDir(runRoot)
		if err != nil {
			t.Fatal(err)
		}
		spec = &localsandbox.Spec{RunRoot: runRoot, ClaudeProjectDir: projectDir}
	}

	var (
		mu      sync.Mutex
		reports []agentproc.PluginsReport
	)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sink := &probeSink{results: make(chan *domain.Message, 16)}
	lr, err := agentproc.RunInteractive(ctx, agentproc.RunOptions{
		Cwd:          runRoot,
		Model:        "haiku",
		AllowedTools: "Bash",
		Message:      "Use the Bash tool to run exactly this command: echo tf-plugin-probe-deny\nThen reply with the single word DONE.",
		TraceID:      "live-denyplugin",
		Plugins:      plugins,
		LocalSandbox: spec,
		OnPluginsLoaded: func(r agentproc.PluginsReport) {
			mu.Lock()
			reports = append(reports, r)
			mu.Unlock()
		},
	}, sink, nil)
	if err != nil {
		t.Fatalf("RunInteractive: %v", err)
	}
	defer func() { _ = lr.Close() }()

	deadline := time.After(3 * time.Minute)
	for {
		select {
		case m := <-sink.results:
			if !m.IsError || !strings.Contains(m.Content, "denyplugin refused this command") {
				continue
			}
			mu.Lock()
			got := append([]agentproc.PluginsReport(nil), reports...)
			mu.Unlock()
			if len(got) == 0 || !got[0].OK || len(got[0].Plugins) != 1 || got[0].Plugins[0].Name != "denyplugin" {
				t.Fatalf("plugins_loaded reports = %+v, want one passing report naming denyplugin", got)
			}
			t.Logf("denied as expected; plugins loaded %v after spawn (init follows the first user message)", got[0].SinceSpawn)
			return
		case <-lr.Done():
			t.Fatalf("run ended before the plugin's deny was seen: err=%v stderr=%s", lr.Err(), lr.Stderr())
		case <-deadline:
			t.Fatalf("timed out waiting for the plugin's deny (stderr: %s)", lr.Stderr())
		}
	}
}

// probeSink hands every tool result to the test.
type probeSink struct {
	results chan *domain.Message
}

func (s *probeSink) OnSession(string) error { return nil }

func (s *probeSink) OnMessage(m *domain.Message) error {
	if m.Role == "tool" || m.ToolCallID != "" {
		select {
		case s.results <- m:
		default:
		}
	}
	return nil
}
