package agentproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pluginScenarioStubSDK stands in for the SDK to drive the plugin latch: it
// runs SCENARIO, a list of steps, in order. "user" waits for the next user
// message, "drain" waits for the input to end, "init" yields a system/init
// naming plugins and plugin_errors, "pre" calls the PreToolUse hook and
// yields its answer, "can" calls canUseTool (null when it is not wired),
// "nocb" makes stdout drop every write callback, and "hang" never returns.
const pluginScenarioStubSDK = `
const SCENARIO = %s
export function query({ prompt, options }) {
  const it = prompt?.[Symbol.asyncIterator]?.()
  return {
    async *[Symbol.asyncIterator]() {
      const signal = new AbortController().signal
      const pre = options.hooks?.PreToolUse?.[0]?.hooks?.[0]
      for (const step of SCENARIO) {
        switch (step.op) {
          case "user":
            await it.next()
            break
          case "drain":
            for (;;) {
              const r = await it.next()
              if (r.done) break
            }
            yield { type: "stub_drained" }
            break
          case "init": {
            const init = { type: "system", subtype: "init", session_id: "sess-1", plugins: step.plugins ?? [] }
            if (step.errors) init.plugin_errors = step.errors
            yield init
            break
          }
          case "pre": {
            const call = { hook_event_name: "PreToolUse", tool_name: step.tool ?? "Bash", tool_input: {}, tool_use_id: step.id }
            if (step.agent) call.agent_id = step.agent
            yield { type: "stub_pre", id: step.id, output: await pre(call, step.id, { signal }) }
            break
          }
          case "can":
            yield {
              type: "stub_can",
              id: step.id,
              output: options.canUseTool ? await options.canUseTool("Write", {}, { toolUseID: step.id, signal }) : null,
            }
            break
          case "nocb": {
            // Drop every write callback from here on, data still written.
            const write = process.stdout.write.bind(process.stdout)
            process.stdout.write = (chunk) => write(chunk)
            break
          }
          case "hang":
            await new Promise(() => {})
            break
        }
      }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

type step = map[string]any

// pluginWrapper is a running wrapper.mjs under a scenario stub, with the
// process handle kept so a test can read its exit status.
type pluginWrapper struct {
	*wrapperProc
	cmd      *exec.Cmd
	waitOnce sync.Once
	waitErr  error
}

func (p *pluginWrapper) wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

// startPluginWrapper starts the shipped wrapper in streaming-input mode under
// the scenario stub. Permission prompts are on only when asked for, because
// an allowlisted call with them off reaches no canUseTool and the latch must
// still hold.
func startPluginWrapper(t *testing.T, node string, scenario []step, permissionPrompts bool, extraArgs ...string) *pluginWrapper {
	t.Helper()
	cmd, stdin, stderr := pluginWrapperCmd(t, node, scenario, permissionPrompts, extraArgs...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	lines := bufio.NewScanner(out)
	lines.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	pw := &pluginWrapper{wrapperProc: &wrapperProc{stdin: stdin, stderr: stderr, lines: lines, t: t}, cmd: cmd}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = pw.wait()
	})
	return pw
}

func pluginWrapperCmd(t *testing.T, node string, scenario []step, permissionPrompts bool, extraArgs ...string) (*exec.Cmd, io.WriteCloser, *syncBuffer) {
	t.Helper()
	encoded, err := json.Marshal(scenario)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir stub: %v", err)
	}
	writeFile(t, filepath.Join(pkgDir, "package.json"),
		`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.0.0-stub","type":"module","main":"index.mjs"}`)
	writeFile(t, filepath.Join(pkgDir, "index.mjs"), fmt.Sprintf(pluginScenarioStubSDK, encoded))
	wrapper := filepath.Join(dir, "wrapper.mjs")
	writeFile(t, wrapper, string(wrapperJS))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	args := []string{wrapper, "--input-format", "stream-json"}
	if permissionPrompts {
		args = append(args, "--permission-prompts")
	}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stderr := newSyncBuffer()
	cmd.Stderr = stderr
	return cmd, stdin, stderr
}

func (p *pluginWrapper) sendUser() {
	p.t.Helper()
	if _, err := p.stdin.Write([]byte(`{"kind":"user_message","text":"go"}` + "\n")); err != nil {
		p.t.Fatalf("write user_message: %v", err)
	}
}

func (p *pluginWrapper) sendEnd() {
	p.t.Helper()
	if _, err := p.stdin.Write([]byte(`{"kind":"end"}` + "\n")); err != nil {
		p.t.Fatalf("write end: %v", err)
	}
}

// expectExit waits for the process and checks its exit status.
func (p *pluginWrapper) expectExit(code int) {
	p.t.Helper()
	err := p.wait()
	var exitErr *exec.ExitError
	got := 0
	if errors.As(err, &exitErr) {
		got = exitErr.ExitCode()
	} else if err != nil {
		p.t.Fatalf("wait: %v (stderr: %s)", err, p.stderr.String())
	}
	if got != code {
		p.t.Fatalf("exit status = %d, want %d (stderr: %s)", got, code, p.stderr.String())
	}
}

// makePluginDir creates a directory that looks like a Claude Code plugin.
func makePluginDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"`+name+`","version":"1.2.3"}`)
	return dir
}

func loadedRow(name, path string) map[string]any {
	return map[string]any{"name": name, "version": "1.2.3", "path": path}
}

// expectDeny reads a stub_pre answer and checks it is a PreToolUse deny with
// a reason containing want.
func expectDeny(t *testing.T, msg map[string]any, want string) {
	t.Helper()
	output, _ := msg["output"].(map[string]any)
	specific, _ := output["hookSpecificOutput"].(map[string]any)
	if msg["type"] != "stub_pre" || specific["permissionDecision"] != "deny" ||
		!strings.Contains(fmt.Sprint(specific["permissionDecisionReason"]), want) {
		t.Fatalf("got %v, want a PreToolUse deny containing %q", msg, want)
	}
}

// expectPluginsLoaded reads lines up to the next plugins_loaded report,
// skipping the init line it follows, and checks ok.
func expectPluginsLoaded(t *testing.T, w *pluginWrapper, ok bool) map[string]any {
	t.Helper()
	for {
		msg := w.next()
		if msg["type"] == "system" && msg["subtype"] == "init" {
			continue
		}
		if msg["type"] != "control" || msg["subtype"] != "plugins_loaded" {
			t.Fatalf("got %v, want a plugins_loaded report (stderr: %s)", msg, w.stderr.String())
		}
		if msg["ok"] != ok {
			t.Fatalf("plugins_loaded ok = %v, want %v: %v", msg["ok"], ok, msg)
		}
		return msg
	}
}

func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	return node
}

// TestWrapperPluginsReachSDKOptions pins the plugins option: one
// {type:"local", path, skipMcpDiscovery:true} per --plugin-dir, in order, and
// no plugins key at all without the flag.
func TestWrapperPluginsReachSDKOptions(t *testing.T) {
	node := requireNode(t)
	w := startWrapper(t, node, optionsEchoStubSDK, "--plugin-dir", "/p/one", "--plugin-dir", "/p/two")
	msg := w.next()
	got, _ := msg["plugins"].([]any)
	if len(got) != 2 {
		t.Fatalf("plugins = %#v, want two entries", msg["plugins"])
	}
	for i, path := range []string{"/p/one", "/p/two"} {
		entry, _ := got[i].(map[string]any)
		if entry["type"] != "local" || entry["path"] != path || entry["skipMcpDiscovery"] != true {
			t.Errorf("plugins[%d] = %#v, want {type:local, path:%s, skipMcpDiscovery:true}", i, entry, path)
		}
	}
	for _, flag := range []string{"--plugin-dir"} {
		if strings.Contains(w.stderr.String(), "ignoring unknown arg "+flag+"\n") {
			t.Errorf("wrapper did not recognize %s: %s", flag, w.stderr.String())
		}
	}

	none := startWrapper(t, node, optionsEchoStubSDK).next()
	if _, present := none["plugins"]; present && none["plugins"] != nil {
		t.Errorf("plugins = %#v with no --plugin-dir, want absent", none["plugins"])
	}
}

// TestWrapperPluginLatchDeniesUntilInitThenFails: with a required plugin the
// first tool call is denied while the check is pending, an init that does not
// list the plugin produces plugins_loaded ok:false naming it, and the process
// exits with status 3 once that line is written.
func TestWrapperPluginLatchDeniesUntilInitThenFails(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")
	w := startPluginWrapper(t, node, []step{
		{"op": "user"},
		{"op": "pre", "id": "toolu_1"},
		{"op": "init", "plugins": []any{}},
		{"op": "hang"},
	}, true, "--plugin-dir", guard, "--plugin-required", guard)
	w.sendUser()
	expectDeny(t, w.next(), "required plugin check pending")
	report := expectPluginsLoaded(t, w, false)
	failed, _ := report["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed = %#v, want the one required plugin", report["failed"])
	}
	entry, _ := failed[0].(map[string]any)
	if entry["required"] != true || !strings.Contains(fmt.Sprint(entry["reason"]), "did not report it loaded") {
		t.Errorf("failed[0] = %#v, want the required entry with its reason", entry)
	}
	w.expectExit(pluginExitCodeForTest)
}

// pluginExitCodeForTest mirrors wrapper.mjs's pluginExitCode.
const pluginExitCodeForTest = 3

// TestWrapperPluginLatchSurvivesShutdown: ending the input sets the wrapper's
// closing state, which answers every other hook call with nothing; a tool call
// after that must still be denied while the latch is closed.
func TestWrapperPluginLatchSurvivesShutdown(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")
	w := startPluginWrapper(t, node, []step{
		{"op": "user"},
		{"op": "drain"},
		{"op": "pre", "id": "toolu_after_end"},
	}, true, "--plugin-dir", guard, "--plugin-required", guard)
	w.sendUser()
	w.sendEnd()
	if msg := w.next(); msg["type"] != "stub_drained" {
		t.Fatalf("got %v, want the stub to see the input end", msg)
	}
	expectDeny(t, w.next(), "required plugin check pending")
}

// TestWrapperPluginLatchCoversEveryCall: a subagent's call (agent_id set) and
// a main-thread call with permission prompts off (an allowlisted call that
// never reaches canUseTool) are both denied by the hook, and canUseTool, where
// it is wired, checks the latch too.
func TestWrapperPluginLatchCoversEveryCall(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")

	t.Run("permission prompts off", func(t *testing.T) {
		w := startPluginWrapper(t, node, []step{
			{"op": "pre", "id": "toolu_sub", "agent": "sub-1", "tool": "Read"},
			{"op": "pre", "id": "toolu_main"},
			{"op": "can", "id": "toolu_can"},
		}, false, "--plugin-dir", guard, "--plugin-required", guard)
		expectDeny(t, w.next(), "required plugin check pending")
		expectDeny(t, w.next(), "required plugin check pending")
		if msg := w.next(); msg["type"] != "stub_can" || msg["output"] != nil {
			t.Fatalf("got %v, want no canUseTool without --permission-prompts", msg)
		}
	})

	t.Run("canUseTool wired", func(t *testing.T) {
		w := startPluginWrapper(t, node, []step{
			{"op": "can", "id": "toolu_can"},
		}, true, "--plugin-dir", guard, "--plugin-required", guard)
		msg := w.next()
		output, _ := msg["output"].(map[string]any)
		if msg["type"] != "stub_can" || output["behavior"] != "deny" || output["message"] != "required plugin check pending" {
			t.Fatalf("got %v, want canUseTool to deny on the latch", msg)
		}
	})
}

// TestWrapperPluginLatchTiesErrors: a required plugin that loaded but carries
// a load error fails, whether the error names the plugin (a hook that did not
// load, with no path) by manifest or directory name, names its path, carries
// the positional inline[N] tag of an entry that failed before it had a name,
// or is an inline error that names no configured entry at all.
func TestWrapperPluginLatchTiesErrors(t *testing.T) {
	node := requireNode(t)
	other := makePluginDir(t, "other")
	guard := makePluginDir(t, "guard")
	cases := []struct {
		name   string
		rows   []any
		errors []any
	}{
		{
			name:   "by plugin name",
			rows:   []any{loadedRow("other", other), loadedRow("guard", guard)},
			errors: []any{map[string]any{"plugin": "guard@inline", "type": "hook-load-failed", "message": "hooks.json is invalid"}},
		},
		{
			name:   "by path",
			rows:   []any{loadedRow("other", other)},
			errors: []any{map[string]any{"plugin": "inline[9]", "type": "manifest-validation-error", "message": "bad", "path": guard}},
		},
		{
			name:   "by inline index",
			rows:   []any{loadedRow("other", other)},
			errors: []any{map[string]any{"plugin": "inline[1]", "type": "generic-error", "message": "failed before it had a name"}},
		},
		{
			// Claude Code tags a component that failed to load with the
			// plugin's directory name, not its manifest name.
			name:   "by directory name",
			rows:   []any{loadedRow("other", other), loadedRow("guard-manifest-name", guard)},
			errors: []any{map[string]any{"plugin": filepath.Base(guard) + "@inline", "type": "hook-load-failed", "message": "hooks.json is invalid"}},
		},
		{
			// An inline error that names no configured entry still belongs to
			// one of them, so it fails the required one.
			name:   "unattributed inline error",
			rows:   []any{loadedRow("other", other), loadedRow("guard", guard)},
			errors: []any{map[string]any{"plugin": "somethingelse@inline", "type": "hook-load-failed", "message": "?"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := startPluginWrapper(t, node, []step{
				{"op": "user"},
				{"op": "init", "plugins": tc.rows, "errors": tc.errors},
				{"op": "hang"},
			}, true, "--plugin-dir", other, "--plugin-dir", guard, "--plugin-required", guard)
			w.sendUser()
			report := expectPluginsLoaded(t, w, false)
			failed, _ := report["failed"].([]any)
			found := false
			for _, f := range failed {
				entry, _ := f.(map[string]any)
				if entry["required"] == true {
					found = true
				}
			}
			if !found {
				t.Fatalf("failed = %#v, want the required guard listed", report["failed"])
			}
			w.expectExit(pluginExitCodeForTest)
		})
	}
}

// TestWrapperPluginLatchMatchesByFileIdentity: Claude Code reports a
// normalised form of the path it was given, so the match is by file identity,
// not by string. A path through a symlink, or spelled with a "..", opens the
// latch, and a call after it goes through.
func TestWrapperPluginLatchMatchesByFileIdentity(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")
	variants := map[string]string{
		"dot-dot": filepath.Join(guard, "..", filepath.Base(guard)) + string(filepath.Separator),
	}
	link := filepath.Join(t.TempDir(), "guard-link")
	if err := os.Symlink(guard, link); err == nil {
		variants["symlink"] = link
	}
	if upper := strings.ToUpper(guard); upper != guard {
		if _, err := os.Stat(upper); err == nil {
			variants["case"] = upper
		}
	}
	for name, reported := range variants {
		t.Run(name, func(t *testing.T) {
			w := startPluginWrapper(t, node, []step{
				{"op": "user"},
				{"op": "init", "plugins": []any{loadedRow("guard", reported)}},
				{"op": "pre", "id": "toolu_after"},
			}, true, "--plugin-dir", guard, "--plugin-required", guard)
			w.sendUser()
			report := expectPluginsLoaded(t, w, true)
			plugins, _ := report["plugins"].([]any)
			if len(plugins) != 1 {
				t.Fatalf("plugins = %#v, want the guard", report["plugins"])
			}
			entry, _ := plugins[0].(map[string]any)
			if entry["name"] != "guard" || entry["version"] != "1.2.3" || entry["required"] != true {
				t.Errorf("plugins[0] = %#v, want guard 1.2.3, required", entry)
			}
			if msg := w.next(); msg["type"] != "control" || msg["subtype"] != "tool_started" || msg["tool_call_id"] != "toolu_after" {
				t.Fatalf("got %v, want the call after a passing init to go through", msg)
			}
		})
	}
}

// TestWrapperPluginLatchInitDeadline: with no init before the deadline, which
// starts at the first user message, the check fails and the process exits 3.
func TestWrapperPluginLatchInitDeadline(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")
	w := startPluginWrapper(t, node, []step{
		{"op": "user"},
		{"op": "hang"},
	}, true, "--plugin-dir", guard, "--plugin-required", guard, "--plugin-init-timeout-ms", "300")
	start := time.Now()
	w.sendUser()
	report := expectPluginsLoaded(t, w, false)
	if !strings.Contains(fmt.Sprint(report["failed"]), "did not report its loaded plugins") {
		t.Errorf("failed = %v, want the deadline's reason", report["failed"])
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("failed after %v, before the 300ms deadline", elapsed)
	}
	w.expectExit(pluginExitCodeForTest)
}

// TestWrapperPluginLatchReEmittedInit: a second init that no longer lists the
// required plugin closes the latch and fails the run, after a first that
// passed.
func TestWrapperPluginLatchReEmittedInit(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")
	w := startPluginWrapper(t, node, []step{
		{"op": "user"},
		{"op": "init", "plugins": []any{loadedRow("guard", guard)}},
		{"op": "init", "plugins": []any{}},
		{"op": "hang"},
	}, true, "--plugin-dir", guard, "--plugin-required", guard)
	w.sendUser()
	expectPluginsLoaded(t, w, true)
	expectPluginsLoaded(t, w, false)
	w.expectExit(pluginExitCodeForTest)
}

// TestWrapperPluginsNotRequired: with no required plugin the latch is open
// from the start, and the report still arrives, listing a plugin that did not
// load as failed with ok:true.
func TestWrapperPluginsNotRequired(t *testing.T) {
	node := requireNode(t)
	loaded := makePluginDir(t, "loaded")
	missing := makePluginDir(t, "missing")
	w := startPluginWrapper(t, node, []step{
		{"op": "pre", "id": "toolu_before"},
		{"op": "init", "plugins": []any{loadedRow("loaded", loaded)}},
	}, true, "--plugin-dir", loaded, "--plugin-dir", missing)
	if msg := w.next(); msg["type"] != "control" || msg["subtype"] != "tool_started" {
		t.Fatalf("got %v, want a call before init to go through with no required plugin", msg)
	}
	if msg := w.next(); msg["type"] != "stub_pre" {
		t.Fatalf("got %v, want the hook's answer", msg)
	}
	report := expectPluginsLoaded(t, w, true)
	failed, _ := report["failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("failed = %#v, want the plugin that did not load", report["failed"])
	}
}

// TestWrapperPluginFailureExitFallback: when the failure line's write
// callback never fires, the 2-second fallback still ends the process with
// status 3. The stub makes stdout drop its callbacks before the failing init.
func TestWrapperPluginFailureExitFallback(t *testing.T) {
	node := requireNode(t)
	guard := makePluginDir(t, "guard")
	w := startPluginWrapper(t, node, []step{
		{"op": "user"},
		{"op": "nocb"},
		{"op": "init", "plugins": []any{}},
		{"op": "hang"},
	}, true, "--plugin-dir", guard, "--plugin-required", guard)
	w.sendUser()
	start := time.Now()
	expectPluginsLoaded(t, w, false)
	w.expectExit(pluginExitCodeForTest)
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Errorf("exited after %v; with no write callback the exit should wait for the 2s fallback", elapsed)
	}
}

// TestWrapperOneShotRefusesPlugins: the check lives in streaming-input mode,
// so the one-shot path refuses a plugin rather than load it unchecked.
func TestWrapperOneShotRefusesPlugins(t *testing.T) {
	node := requireNode(t)
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pkgDir, "package.json"),
		`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.0.0-stub","type":"module","main":"index.mjs"}`)
	writeFile(t, filepath.Join(pkgDir, "index.mjs"), optionsEchoStubSDK)
	wrapper := filepath.Join(dir, "wrapper.mjs")
	writeFile(t, wrapper, string(wrapperJS))
	cmd := exec.Command(node, wrapper, "-p", "hi", "--plugin-dir", "/p/one")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(string(out), "--plugin-dir needs --input-format stream-json") {
		t.Fatalf("one-shot with --plugin-dir: err=%v out=%s, want exit 2 naming streaming-input", err, out)
	}
}
