package agentproc

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs_InitialInvocation(t *testing.T) {
	got := BuildArgs(RunOptions{
		Cwd:          "/tmp/triagefactory-runs/rk1",
		Message:      "do the thing",
		Model:        "sonnet-4-6",
		AllowedTools: "Read,Write",
		MaxTurns:     100,
	})
	want := []string{
		"-p", "do the thing",
		"--model", "sonnet-4-6",
		"--permission-mode", "default",
		"--setting-sources", "project",
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", "Read,Write",
		"--disallowedTools", "AskUserQuestion",
		"--max-turns", "100",
	}
	if !slices.Equal(got, want) {
		t.Errorf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_Interactive(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive:   true,
		Message:       "this is sent over stdin, not argv",
		OpeningBlocks: []ContentBlock{{Type: "text", Text: "this block is sent over stdin too"}},
		Model:         "sonnet-4-6",
		AllowedTools:  "Read,Write",
		MaxTurns:      50,
	})

	// Streaming-input mode must request the stream-json input format and
	// must NOT carry the one-shot -p prompt (the opening message goes
	// over stdin once the wrapper signals ready, in either spelling).
	if slices.Contains(got, "-p") {
		t.Errorf("interactive argv must omit -p: %v", got)
	}
	if slices.Contains(got, "this is sent over stdin, not argv") {
		t.Errorf("interactive argv must omit Message: %v", got)
	}
	for _, arg := range got {
		if strings.Contains(arg, "this block is sent over stdin too") {
			t.Errorf("interactive argv must omit OpeningBlocks: %v", got)
		}
	}
	ifIdx := slices.Index(got, "--input-format")
	if ifIdx < 0 {
		t.Fatalf("expected --input-format flag: %v", got)
	}
	if got[ifIdx+1] != "stream-json" {
		t.Errorf("--input-format value = %q, want stream-json", got[ifIdx+1])
	}

	// The shared flags still flow through unchanged.
	if !slices.Contains(got, "--output-format") || !slices.Contains(got, "--verbose") {
		t.Errorf("missing fixed flags: %v", got)
	}
	if !slices.Contains(got, "--allowedTools") || !slices.Contains(got, "--max-turns") {
		t.Errorf("expected allowedTools + max-turns to flow through: %v", got)
	}
}

func TestBuildArgs_PermissionPromptsFlag(t *testing.T) {
	// The opt-in flag only appears in interactive mode and only when the
	// caller asked for it. Its presence is what makes the wrapper wire
	// canUseTool; its absence keeps the wrapper allowlist-only.
	withHandler := BuildArgs(RunOptions{Interactive: true, PermissionPrompts: true, Model: "sonnet-4-6"})
	if !slices.Contains(withHandler, "--permission-prompts") {
		t.Errorf("expected --permission-prompts when PermissionPrompts=true: %v", withHandler)
	}

	withoutHandler := BuildArgs(RunOptions{Interactive: true, PermissionPrompts: false, Model: "sonnet-4-6"})
	if slices.Contains(withoutHandler, "--permission-prompts") {
		t.Errorf("expected --permission-prompts omitted when PermissionPrompts=false: %v", withoutHandler)
	}

	// One-shot mode never wires canUseTool, so the flag is inert there and
	// must not be emitted even if the field is somehow set.
	oneShot := BuildArgs(RunOptions{Interactive: false, PermissionPrompts: true, Message: "hi"})
	if slices.Contains(oneShot, "--permission-prompts") {
		t.Errorf("expected --permission-prompts omitted in one-shot mode: %v", oneShot)
	}
}

// TestBuildArgs_ToolBatchNotesFlag: --tool-batch-notes is passed only for an
// interactive run whose caller answers a tool batch.
func TestBuildArgs_ToolBatchNotesFlag(t *testing.T) {
	answer := func() ToolBatchNotes { return ToolBatchNotes{} }
	if args := BuildArgs(RunOptions{Interactive: true, OnToolBatch: answer}); !slices.Contains(args, "--tool-batch-notes") {
		t.Errorf("expected --tool-batch-notes with a tool-batch answer: %v", args)
	}
	if args := BuildArgs(RunOptions{Interactive: true}); slices.Contains(args, "--tool-batch-notes") {
		t.Errorf("expected --tool-batch-notes omitted with no answer: %v", args)
	}
	if args := BuildArgs(RunOptions{OnToolBatch: answer, Message: "hi"}); slices.Contains(args, "--tool-batch-notes") {
		t.Errorf("expected --tool-batch-notes omitted in one-shot mode: %v", args)
	}
}

func TestBuildArgs_DisallowsAskUserQuestion(t *testing.T) {
	// AskUserQuestion is removed from the model's context on every path,
	// not just left off the allowlist: one-shot, interactive, and resume
	// all carry the flag, and it never depends on caller-supplied fields.
	cases := map[string]RunOptions{
		"one-shot":    {Message: "hi"},
		"interactive": {Interactive: true, PermissionPrompts: true},
		"resume":      {Message: "hi", SessionID: "sess-123"},
	}
	for name, opts := range cases {
		got := BuildArgs(opts)
		idx := slices.Index(got, "--disallowedTools")
		if idx < 0 || idx+1 >= len(got) {
			t.Fatalf("%s: expected --disallowedTools flag: %v", name, got)
		}
		if got[idx+1] != "AskUserQuestion" {
			t.Errorf("%s: --disallowedTools value = %q, want AskUserQuestion", name, got[idx+1])
		}
	}
}

// TestBuildArgs_LoadsOnlyProjectSettings pins that no spawn loads the
// operator's user settings: one-shot, interactive and resume runs name the
// project source alone, and a spawn with no cwd of its own names none, since
// its cwd is wherever TF was started and may be the operator's home. Left to
// Claude Code's default, the operator's permission rules would merge with the
// allowlist.
func TestBuildArgs_LoadsOnlyProjectSettings(t *testing.T) {
	const cwd = "/tmp/triagefactory-runs/rk1"
	cases := map[string]struct {
		opts RunOptions
		want string
	}{
		"one-shot":    {RunOptions{Cwd: cwd, Message: "hi"}, "project"},
		"interactive": {RunOptions{Cwd: cwd, Interactive: true, PermissionPrompts: true}, "project"},
		"resume":      {RunOptions{Cwd: cwd, Message: "hi", SessionID: "sess-123"}, "project"},
		"no cwd":      {RunOptions{Message: "score these"}, ""},
	}
	for name, tc := range cases {
		got := BuildArgs(tc.opts)
		idx := slices.Index(got, "--setting-sources")
		if idx < 0 || idx+1 >= len(got) {
			t.Fatalf("%s: expected --setting-sources flag: %v", name, got)
		}
		if got[idx+1] != tc.want {
			t.Errorf("%s: --setting-sources value = %q, want %q", name, got[idx+1], tc.want)
		}
		if slices.Contains(got, "--settings") {
			t.Errorf("%s: BuildArgs emits --settings; only the spawn adds it, and only without a resolved credential: %v", name, got)
		}
	}
}

func TestBuildArgs_PermissionMode(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive:    true,
		Model:          "sonnet-4-6",
		PermissionMode: "auto",
	})
	idx := slices.Index(got, "--permission-mode")
	if idx < 0 || idx+1 >= len(got) {
		t.Fatalf("expected --permission-mode flag: %v", got)
	}
	if got[idx+1] != "auto" {
		t.Errorf("--permission-mode value = %q, want auto", got[idx+1])
	}

	// An omitted mode is Claude Code's to choose, and it can choose auto, so
	// the zero value must still put "default" on the wire — for one-shot
	// runs (the toolless system jobs) as much as interactive ones.
	for name, opts := range map[string]RunOptions{
		"interactive": {Interactive: true},
		"one-shot":    {Message: "hi"},
	} {
		withoutMode := BuildArgs(opts)
		idx := slices.Index(withoutMode, "--permission-mode")
		if idx < 0 || idx+1 >= len(withoutMode) {
			t.Fatalf("%s: zero-value PermissionMode must still send the flag: %v", name, withoutMode)
		}
		if withoutMode[idx+1] != "default" {
			t.Errorf("%s: --permission-mode value = %q, want default", name, withoutMode[idx+1])
		}
	}
}

func TestBuildArgs_ResumeAddsResumeFlagBeforeModel(t *testing.T) {
	// --resume must precede --model so claude treats this as a
	// continuation of the captured session, not a fresh run that
	// happens to share a model. Order matters to claude's flag
	// parsing in -p mode.
	got := BuildArgs(RunOptions{
		Message:      "answer to your question",
		SessionID:    "sess-123",
		Model:        "sonnet-4-6",
		AllowedTools: "Read",
	})
	resumeIdx := slices.Index(got, "--resume")
	modelIdx := slices.Index(got, "--model")
	if resumeIdx < 0 || modelIdx < 0 {
		t.Fatalf("missing flags: %v", got)
	}
	if resumeIdx > modelIdx {
		t.Errorf("--resume must come before --model: %v", got)
	}
	if got[resumeIdx+1] != "sess-123" {
		t.Errorf("resume id = %q, want sess-123", got[resumeIdx+1])
	}
}

func TestBuildArgs_OmitsZeroValueFlags(t *testing.T) {
	got := BuildArgs(RunOptions{Message: "hi"})
	for _, flag := range []string{"--model", "--resume", "--allowedTools", "--max-turns", "--add-dir"} {
		if slices.Contains(got, flag) {
			t.Errorf("expected %q to be omitted, got %v", flag, got)
		}
	}
	if !slices.Contains(got, "--output-format") || !slices.Contains(got, "--verbose") {
		t.Errorf("missing fixed flags: %v", got)
	}
}

func TestBuildArgs_AddDirsEmitsOnePerEntry(t *testing.T) {
	// --add-dir is repeatable; CC's path-scoped tool checks (notably
	// the rm guard) treat each entry as an independent allowed dir
	// rather than parsing a comma-joined list. Empty strings are
	// dropped so callers can pass a slice with conditional entries
	// without filtering at the call site.
	got := BuildArgs(RunOptions{
		Message: "hi",
		AddDirs: []string{"/a", "", "/b/c"},
	})
	flags := []int{}
	for i, v := range got {
		if v == "--add-dir" {
			flags = append(flags, i)
		}
	}
	if len(flags) != 2 {
		t.Fatalf("expected 2 --add-dir entries, got %d: %v", len(flags), got)
	}
	if got[flags[0]+1] != "/a" || got[flags[1]+1] != "/b/c" {
		t.Errorf("add-dir values wrong: %v", got)
	}
}

// pluginFlagNames are every flag the plugin half of the argv can emit.
var pluginFlagNames = []string{"--plugin-dir", "--plugin-required", "--plugin-init-timeout-ms"}

// flagValues returns the operand of each occurrence of flag, in order.
func flagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func requireNoPluginFlags(t *testing.T, args []string) {
	t.Helper()
	for _, flag := range pluginFlagNames {
		if slices.Contains(args, flag) {
			t.Errorf("argv carries %s with no plugin to name: %v", flag, args)
		}
	}
}

func TestBuildArgs_NoPluginsEmitsNoPluginFlags(t *testing.T) {
	for name, opts := range map[string]RunOptions{
		"one-shot":                      {Message: "m"},
		"interactive":                   {Interactive: true},
		"nil list with a timeout":       {Interactive: true, PluginInitTimeout: time.Minute},
		"empty list with a timeout":     {Interactive: true, Plugins: []Plugin{}, PluginInitTimeout: time.Minute},
		"one-shot with add-dirs":        {Message: "m", AddDirs: []string{"/a"}},
		"interactive with a state path": {Interactive: true, Cwd: "/tmp/run"},
	} {
		t.Run(name, func(t *testing.T) {
			requireNoPluginFlags(t, BuildArgs(opts))
		})
	}

	// An empty list changes nothing, byte for byte.
	base := RunOptions{Interactive: true, Model: "m", AllowedTools: "Read", AddDirs: []string{"/a"}, SystemPrompt: "sp", MaxTurns: 3}
	withEmpty := base
	withEmpty.Plugins = []Plugin{}
	if got, want := BuildArgs(withEmpty), BuildArgs(base); !slices.Equal(got, want) {
		t.Errorf("an empty plugin list changed the argv\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_PluginsEmitOneDirPerEntryInOrder(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive: true,
		Plugins: []Plugin{
			{Path: "/p/a"},
			{Path: "/p/b", Required: true},
			{Path: "/p/c"},
			{Path: "/p/d", Required: true},
		},
	})
	if dirs := flagValues(got, "--plugin-dir"); !slices.Equal(dirs, []string{"/p/a", "/p/b", "/p/c", "/p/d"}) {
		t.Errorf("--plugin-dir values = %v", dirs)
	}
	if req := flagValues(got, "--plugin-required"); !slices.Equal(req, []string{"/p/b", "/p/d"}) {
		t.Errorf("--plugin-required values = %v, want only the required entries", req)
	}
	if slices.Contains(got, "--plugin-init-timeout-ms") {
		t.Errorf("a timeout flag without a configured timeout: %v", got)
	}
}

func TestBuildArgs_PluginsWithNoRequiredEntryEmitNoRequiredFlag(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive:       true,
		Plugins:           []Plugin{{Path: "/p/a"}, {Path: "/p/b"}},
		PluginInitTimeout: time.Minute,
	})
	if slices.Contains(got, "--plugin-required") {
		t.Errorf("--plugin-required with no required plugin: %v", got)
	}
	if slices.Contains(got, "--plugin-init-timeout-ms") {
		t.Errorf("an init deadline for a run that waits on nothing: %v", got)
	}
	if dirs := flagValues(got, "--plugin-dir"); len(dirs) != 2 {
		t.Errorf("--plugin-dir values = %v, want both", dirs)
	}
}

func TestBuildArgs_PluginsDropEmptyPaths(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive: true,
		Cwd:         "/tmp/run",
		Plugins:     []Plugin{{Path: ""}, {Path: "/p/a"}, {Path: "", Required: true}},
	})
	if dirs := flagValues(got, "--plugin-dir"); !slices.Equal(dirs, []string{"/p/a"}) {
		t.Errorf("--plugin-dir values = %v, want [/p/a]", dirs)
	}
	if slices.Contains(got, "--plugin-required") {
		t.Errorf("a required entry with an empty path was named: %v", got)
	}
	for i, a := range got {
		if a == "" {
			t.Errorf("empty argv entry at %d: %v", i, got)
		}
	}

	onlyEmpty := BuildArgs(RunOptions{
		Interactive:       true,
		Plugins:           []Plugin{{Path: "", Required: true}},
		PluginInitTimeout: time.Minute,
	})
	requireNoPluginFlags(t, onlyEmpty)
}

func TestBuildArgs_PluginInitTimeoutNeedsARequiredEntryAndAPositiveValue(t *testing.T) {
	required := []Plugin{{Path: "/p/a", Required: true}}
	cases := []struct {
		name    string
		plugins []Plugin
		timeout time.Duration
		want    string // empty means no flag
	}{
		{"required with 90s", required, 90 * time.Second, "90000"},
		{"required with a fractional second", required, 1500 * time.Millisecond, "1500"},
		{"required with zero", required, 0, ""},
		{"required with a negative value", required, -time.Second, ""},
		{"not required with 90s", []Plugin{{Path: "/p/a"}}, 90 * time.Second, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildArgs(RunOptions{Interactive: true, Plugins: c.plugins, PluginInitTimeout: c.timeout})
			vals := flagValues(got, "--plugin-init-timeout-ms")
			if c.want == "" {
				if len(vals) != 0 {
					t.Errorf("--plugin-init-timeout-ms = %v, want none", vals)
				}
				return
			}
			if !slices.Equal(vals, []string{c.want}) {
				t.Errorf("--plugin-init-timeout-ms = %v, want [%s]", vals, c.want)
			}
		})
	}
}

// The plugin flags sit with the other per-run grants, ahead of the system
// prompt and the turn cap, the order the wrapper's parser does not depend on
// but a reader of a spawn's argv expects.
func TestBuildArgs_PluginFlagsFollowAddDirs(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive:       true,
		AddDirs:           []string{"/a"},
		Plugins:           []Plugin{{Path: "/p/a", Required: true}},
		PluginInitTimeout: time.Minute,
		SystemPrompt:      "sp",
		MaxTurns:          5,
	})
	idx := func(flag string) int { return slices.Index(got, flag) }
	order := []string{"--add-dir", "--plugin-dir", "--plugin-required", "--plugin-init-timeout-ms", "--append-system-prompt"}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) >= idx(order[i]) {
			t.Errorf("%s does not come before %s: %v", order[i-1], order[i], got)
		}
	}
}
