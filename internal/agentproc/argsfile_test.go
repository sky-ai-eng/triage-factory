package agentproc

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// commandArgs is cmd's argv as the wrapper sees it: when the command was
// built with an args file, the file's contents stand in for the flag and path.
// Tests assert on the wrapper's flags through it so they hold on every OS,
// whether the platform passes them on the command line or in a file.
func commandArgs(t *testing.T, cmd *exec.Cmd) []string {
	t.Helper()
	i := slices.Index(cmd.Args, argsFileFlag)
	if i < 0 {
		return cmd.Args
	}
	if i+1 >= len(cmd.Args) {
		t.Fatalf("%s has no path: %v", argsFileFlag, cmd.Args)
	}
	data, err := os.ReadFile(cmd.Args[i+1])
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("args file %s: %v", cmd.Args[i+1], err)
	}
	return append(slices.Clone(cmd.Args[:i]), list...)
}

func TestRewriteArgsFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := []string{
		filepath.Join(dir, "wrapper.mjs"),
		"--model", "x",
		"-p", "quotes \" and \\ backslashes,\nnewlines and ünïcode",
		"--allowedTools", "",
	}

	out, err := rewriteArgsFile(dir, in)
	if err != nil {
		t.Fatalf("rewriteArgsFile: %v", err)
	}
	if len(out) != 3 || out[0] != in[0] || out[1] != "--args-file" {
		t.Fatalf("rewritten argv = %v, want [%s --args-file <path>]", out, in[0])
	}
	if got := filepath.Dir(out[2]); got != dir {
		t.Errorf("args file is in %s, want %s", got, dir)
	}
	data, err := os.ReadFile(out[2])
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("args file is not a JSON array of strings: %v", err)
	}
	if !reflect.DeepEqual(got, in[1:]) {
		t.Errorf("args file holds %q, want %q", got, in[1:])
	}
}

func TestRewriteArgsFile_RefusesAnEmptyArgumentList(t *testing.T) {
	if _, err := rewriteArgsFile(t.TempDir(), nil); err == nil {
		t.Error("rewriteArgsFile accepted an argument list with no wrapper script")
	}
}

func TestRemoveArgsFile_OnlyTouchesTheArgsDirectory(t *testing.T) {
	dir := argsFileDir()
	out, err := rewriteArgsFile(dir, []string{"wrapper.mjs", "-p", "x"})
	if err != nil {
		t.Fatalf("rewriteArgsFile: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "keep.json")
	if err := os.WriteFile(outside, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}

	removeArgsFile(&exec.Cmd{Args: append([]string{"node"}, out...)})
	removeArgsFile(&exec.Cmd{Args: []string{"node", "wrapper.mjs", argsFileFlag, outside}})
	removeArgsFile(&exec.Cmd{Args: []string{"node", "wrapper.mjs", argsFileFlag}}) // no path: no panic
	removeArgsFile(nil)

	if _, err := os.Stat(out[2]); !os.IsNotExist(err) {
		t.Errorf("args file still present after removeArgsFile: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the args directory was touched: %v", err)
	}
	// A second call, as when the wrapper already deleted the file, is quiet.
	removeArgsFile(&exec.Cmd{Args: append([]string{"node"}, out...)})
}

func TestPruneStaleArgsFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "args-stale.json")
	fresh := filepath.Join(dir, "args-fresh.json")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleArgsFileAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	pruneStaleArgsFiles(dir)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale args file survived the prune: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh args file was pruned: %v", err)
	}
}

// argvEchoStubSDK stands in for the SDK to read back what the wrapper
// parsed from its argv: a one-shot query yields one message carrying the
// system prompt append, the model and the prompt, so a test can compare them
// with what Go sent.
const argvEchoStubSDK = `
export function query({ prompt, options }) {
  return {
    async *[Symbol.asyncIterator]() {
      yield {
        type: "stub_options",
        prompt,
        model: options.model,
        append: options.systemPrompt?.append,
      }
    },
  }
}
`

// TestWrapperReceivesALargeSystemPromptThroughNewDirectCommand runs the real
// wrapper under node against a stub SDK with a 48 KB system prompt carried
// the whole way: BuildArgs, newDirectCommand (which on Windows moves the
// arguments into an args file, because a command line there tops out at
// 32,767 characters) and the wrapper's own argument parsing. Elsewhere the
// arguments stay on the command line and the same assertion holds.
func TestWrapperReceivesALargeSystemPromptThroughNewDirectCommand(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("needs node")
	}
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pkgDir, "package.json"),
		`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.0.0-stub","type":"module","main":"index.mjs"}`)
	writeFile(t, filepath.Join(pkgDir, "index.mjs"), argvEchoStubSDK)
	wrapper := filepath.Join(dir, "wrapper.mjs")
	writeFile(t, wrapper, string(wrapperJS))

	unit := "a \"quoted\" line with a \\ backslash and ünïcode\n"
	systemPrompt := strings.Repeat(unit, 48*1024/len(unit)+1)
	if len(systemPrompt) < 48*1024 {
		t.Fatalf("system prompt is %d bytes, want at least 48 KB", len(systemPrompt))
	}
	opts := RunOptions{
		Cwd:          dir,
		Model:        "stub-model",
		Message:      "the opening message",
		SystemPrompt: systemPrompt,
		AllowedTools: "Bash(triagefactory exec *)",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, err := newDirectCommand(ctx, opts, append([]string{wrapper}, BuildArgs(opts)...))
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	defer removeArgsFile(cmd)
	argsFile := slices.Index(cmd.Args, argsFileFlag)
	if useArgsFile && (argsFile != 2 || len(cmd.Args) != 4) {
		t.Fatalf("argv = %.200v, want node, the wrapper, then exactly %s <path>", cmd.Args, argsFileFlag)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("wrapper failed: %v\nstderr: %s", err, stderr.String())
	}

	var got struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
		Append string `json:"append"`
	}
	line, _, _ := strings.Cut(string(out), "\n")
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("wrapper output %q: %v", line, err)
	}
	if got.Append != systemPrompt {
		t.Errorf("wrapper parsed a %d-byte system prompt, want the %d bytes Go sent", len(got.Append), len(systemPrompt))
	}
	if got.Model != "stub-model" || got.Prompt != "the opening message" {
		t.Errorf("wrapper parsed model %q and prompt %q, want the values Go sent", got.Model, got.Prompt)
	}
	// The wrapper deletes the file as soon as it has read it.
	if argsFile >= 0 {
		if _, err := os.Stat(cmd.Args[argsFile+1]); !os.IsNotExist(err) {
			t.Errorf("the wrapper left its args file behind: %v", err)
		}
	}
}
