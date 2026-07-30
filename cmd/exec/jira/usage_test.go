package jira

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// TestHelpTextNamesInvokedPrefix pins the `jira` usage line for each invoked
// form — the applet, the canonical name, and a path (what the SDK sandbox and
// local mode hand the agent). Under the canonical name it is byte-identical to
// what the binary has always printed.
func TestHelpTextNamesInvokedPrefix(t *testing.T) {
	for _, prefix := range []string{"tfac", "triagefactory exec", "./triagefactory exec"} {
		want := "Usage: " + prefix + " jira <resource> <action> [flags]\n"
		if got := helpText(prefix); !strings.HasPrefix(got, want) {
			t.Errorf("helpText(%q) first line = %q, want %q", prefix, firstLine(got), want)
		}
	}
	if strings.Contains(helpText("tfac"), "triagefactory exec") {
		t.Error("applet `jira` help mentions 'triagefactory exec'")
	}
}

// TestTicketHelpUsesResolvedPrefix pins the wiring on the spot-checked verb
// surface: both the resource help and a per-action usage line must name the
// prefix resolved from argv0 (here, the test binary), not a baked-in constant.
//
// host is nil: the help routes answer before any agenthost call, which is what
// makes `jira ticket view --help` work outside a run.
func TestTicketHelpUsesResolvedPrefix(t *testing.T) {
	cases := map[string][]string{
		"resource level": {"--help"},
		"per-action":     {"view", "--help"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() { handleTicket(nil, args) })
			if !strings.Contains(out, prog.Prefix()) {
				t.Errorf("`jira ticket %v` help does not name the resolved prefix %q; got:\n%s", args, prog.Prefix(), out)
			}
			if strings.Contains(out, "triagefactory exec") {
				t.Errorf("`jira ticket %v` help hardcodes 'triagefactory exec'; got:\n%s", args, out)
			}
		})
	}
}

// TestUnknownActionMessageNamesInvokedPrefix pins the mistyped-verb hint. The
// error path exits, so the message is asserted on its builder — the call site
// feeds it prog.Prefix().
func TestUnknownActionMessageNamesInvokedPrefix(t *testing.T) {
	msg := unknownActionMessage("tfac", "vew")
	for _, want := range []string{
		"unknown ticket action: vew",
		"view", // the verb they meant, in the valid list
		"Run 'tfac jira ticket --help' for usage.", // under the invoked name
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if got, want := unknownActionMessage("triagefactory exec", "vew"),
		"Run 'triagefactory exec jira ticket --help' for usage."; !strings.Contains(got, want) {
		t.Errorf("canonical hint changed; want %q in:\n%s", want, got)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	return <-done
}
