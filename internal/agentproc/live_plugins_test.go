package agentproc

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runPluginLines feeds lines to readLoop through a real subprocess that prints
// them and exits with status code, and returns the finished LiveRun with every
// plugins_loaded report it handed out.
func runPluginLines(t *testing.T, code string, lines ...string) (*LiveRun, []PluginsReport) {
	t.Helper()
	return runPluginLinesWith(t, RunOptions{TraceID: "t"}, code, lines...)
}

func runPluginLinesWith(t *testing.T, opts RunOptions, code string, lines ...string) (*LiveRun, []PluginsReport) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	script := `printf '%s\n' "$@"; exit ` + code
	cmd := exec.CommandContext(runCtx, "sh", append([]string{"-c", script, "sh"}, lines...)...)
	proc, err := newExecProc(cmd)
	if err != nil {
		t.Fatalf("newExecProc: %v", err)
	}
	var reports []PluginsReport
	l := &LiveRun{
		cancel:    cancel,
		done:      make(chan struct{}),
		ready:     make(chan struct{}),
		stream:    NewStreamState(),
		plugins:   func(r PluginsReport) { reports = append(reports, r) },
		spawnedAt: time.Now(),
	}
	if err := proc.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go l.readLoop(runCtx, opts, proc, NoopSink{}, nil)
	select {
	case <-l.done:
	case <-time.After(15 * time.Second):
		t.Fatal("readLoop did not finish")
	}
	return l, reports
}

const (
	pluginsFailedLine = `{"type":"control","subtype":"plugins_loaded","ok":false,"failed":[{"path":"/plugins/guard","reason":"Claude Code did not report it loaded","required":true},{"path":"/plugins/extra","reason":"path-not-found: gone"}]}`
	pluginsOKLine     = `{"type":"control","subtype":"plugins_loaded","ok":true,"plugins":[{"name":"guard","version":"1.2.3","path":"/plugins/guard","required":true}]}`
	pluginResultLine  = `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"total_cost_usd":0,"stop_reason":"end_turn","result":"ok"}`
)

// TestLiveRun_PluginNotLoadedFailsTheRun: a plugins_loaded ok:false report
// ends the run with ErrPluginNotLoaded naming the required plugin's path, and
// the report reaches the caller.
func TestLiveRun_PluginNotLoadedFailsTheRun(t *testing.T) {
	l, reports := runPluginLines(t, "3", pluginsFailedLine)
	err := l.Err()
	if !errors.Is(err, ErrPluginNotLoaded) {
		t.Fatalf("Err() = %v, want ErrPluginNotLoaded", err)
	}
	if !strings.HasPrefix(err.Error(), "plugin_not_loaded: ") || !strings.Contains(err.Error(), "/plugins/guard") {
		t.Errorf("Err() = %q, want plugin_not_loaded naming the required path", err)
	}
	if strings.Contains(err.Error(), "/plugins/extra") {
		t.Errorf("Err() = %q names the non-required plugin", err)
	}
	if len(reports) != 1 || reports[0].OK || len(reports[0].Failed) != 2 {
		t.Fatalf("reports = %+v, want one failed report with both entries", reports)
	}
	if reports[0].SinceSpawn < 0 {
		t.Errorf("SinceSpawn = %v, want non-negative", reports[0].SinceSpawn)
	}
}

// TestLiveRun_PluginNotLoadedWinsOverAnEarlierResult: a failure reported by a
// re-emitted init after a turn already produced a result still fails the
// run. Without the override the run would be recorded completed.
func TestLiveRun_PluginNotLoadedWinsOverAnEarlierResult(t *testing.T) {
	l, _ := runPluginLines(t, "3", pluginsOKLine, pluginResultLine, pluginsFailedLine)
	if l.Result() == nil {
		t.Fatal("Result() = nil, want the earlier turn's result kept")
	}
	if err := l.Err(); !errors.Is(err, ErrPluginNotLoaded) {
		t.Fatalf("Err() = %v, want ErrPluginNotLoaded despite the earlier result", err)
	}
}

// TestLiveRun_PluginsLoadedOK: a passing report leaves the run's outcome to
// the run, and reaches the caller with the loaded plugin.
func TestLiveRun_PluginsLoadedOK(t *testing.T) {
	l, reports := runPluginLines(t, "0", pluginsOKLine, pluginResultLine)
	if err := l.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	if len(reports) != 1 || !reports[0].OK || len(reports[0].Plugins) != 1 || reports[0].Plugins[0].Name != "guard" || reports[0].Plugins[0].Version != "1.2.3" {
		t.Fatalf("reports = %+v, want one passing report naming guard 1.2.3", reports)
	}
}

// TestLiveRun_PluginsLoadedWithoutOKReadsAsFailure: a report line with no ok
// field is read as a failure, the safe reading when writer and reader
// disagree about the line.
func TestLiveRun_PluginsLoadedWithoutOKReadsAsFailure(t *testing.T) {
	l, _ := runPluginLines(t, "3", `{"type":"control","subtype":"plugins_loaded","failed":[{"path":"/plugins/guard","reason":"x","required":true}]}`)
	if err := l.Err(); !errors.Is(err, ErrPluginNotLoaded) {
		t.Fatalf("Err() = %v, want ErrPluginNotLoaded", err)
	}
}

// TestParseControlLine_PluginsLoaded pins the decoded report fields.
func TestParseControlLine_PluginsLoaded(t *testing.T) {
	ctl, ok := parseControlLine([]byte(pluginsOKLine))
	if !ok || ctl.Subtype != "plugins_loaded" || ctl.OK == nil || !*ctl.OK {
		t.Fatalf("parseControlLine = %+v, %v; want an ok plugins_loaded", ctl, ok)
	}
	if len(ctl.Plugins) != 1 || ctl.Plugins[0].Path != "/plugins/guard" || !ctl.Plugins[0].Required {
		t.Errorf("Plugins = %+v, want the guard row", ctl.Plugins)
	}
}

// TestRun_RefusesPlugins: the one-shot path has no plugin check, so it
// refuses a call that carries plugins rather than load them unchecked.
func TestRun_RefusesPlugins(t *testing.T) {
	_, err := Run(context.Background(), RunOptions{Message: "hi", Plugins: []Plugin{{Path: "/plugins/guard", Required: true}}}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "interactive mode") {
		t.Fatalf("Run() err = %v, want the interactive-only refusal", err)
	}
}

// TestRunInteractive_RefusesBadPluginInitTimeout: a malformed
// TF_PLUGIN_INIT_TIMEOUT fails a spawn that has a required plugin before
// anything starts.
func TestRunInteractive_RefusesBadPluginInitTimeout(t *testing.T) {
	t.Setenv(envPluginInitTimeout, "soon")
	_, err := RunInteractive(context.Background(), RunOptions{Plugins: []Plugin{{Path: "/plugins/guard", Required: true}}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), envPluginInitTimeout) {
		t.Fatalf("RunInteractive() err = %v, want the timeout refusal", err)
	}
}

// TestLiveRun_PluginExitStatusWithoutAReportFailsTheRun: the wrapper's plugin
// exit status fails a run given plugins even when its report line never
// arrived, and even after an earlier turn's result; a run given none reads the
// same status as an ordinary exit.
func TestLiveRun_PluginExitStatusWithoutAReportFailsTheRun(t *testing.T) {
	withPlugins := RunOptions{TraceID: "t", Plugins: []Plugin{{Path: "/plugins/guard", Required: true}}}
	l, reports := runPluginLinesWith(t, withPlugins, "3", pluginResultLine)
	err := l.Err()
	if !errors.Is(err, ErrPluginNotLoaded) || !strings.Contains(err.Error(), "/plugins/guard") {
		t.Fatalf("Err() = %v, want ErrPluginNotLoaded naming the required plugin", err)
	}
	if len(reports) != 0 {
		t.Errorf("reports = %+v, want none", reports)
	}

	l, _ = runPluginLinesWith(t, RunOptions{TraceID: "t"}, "3", pluginResultLine)
	if err := l.Err(); err != nil {
		t.Errorf("Err() = %v with no plugins, want the earlier result to stand", err)
	}
}
