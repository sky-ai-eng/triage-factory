package jira

import (
	"io"
	"os"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// TestTicketHelpUsesResolvedPrefix pins the help routes on the spot-checked
// verb surface: the resource help and a per-action usage line, each rendered
// under the prefix resolved from argv0 (here, the test binary).
//
// host is nil: the help routes answer before any agenthost call, which is what
// makes `jira ticket view --help` work outside a run.
func TestTicketHelpUsesResolvedPrefix(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"resource level": {[]string{"--help"}, helpText(prog.Prefix())},
		"per-action":     {[]string{"view", "--help"}, "usage: " + prog.Prefix() + " " + ticketHelp["view"] + "\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if out := captureStdout(t, func() { handleTicket(nil, tc.args) }); out != tc.want {
				t.Errorf("`jira ticket %v` printed the wrong help;\ngot  %.200q\nwant %.200q", tc.args, out, tc.want)
			}
		})
	}
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
