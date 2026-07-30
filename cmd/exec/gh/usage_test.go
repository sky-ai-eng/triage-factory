package gh

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// prefixForms are the three shapes a real invocation takes: the applet, the
// canonical name on PATH, and a path (relative or absolute) — the SDK sandbox
// and local mode both hand the agent one of the latter.
var prefixForms = []string{"tfac", "triagefactory exec", "./triagefactory exec", "/opt/tf/run/abc/triagefactory exec"}

// TestPRHelpTextNamesInvokedPrefix pins the `gh pr` usage line for each invoked
// form. Under the canonical name the line is byte-identical to what the binary
// has always printed.
func TestPRHelpTextNamesInvokedPrefix(t *testing.T) {
	for _, prefix := range prefixForms {
		want := "Usage: " + prefix + " gh pr <action> [flags]\n"
		if got := prHelpText(prefix); !strings.HasPrefix(got, want) {
			t.Errorf("prHelpText(%q) first line = %q, want %q", prefix, firstLine(got), want)
		}
	}
	if got := prHelpText("triagefactory exec"); !strings.HasPrefix(got, "Usage: triagefactory exec gh pr <action> [flags]\n") {
		t.Errorf("canonical `gh pr` usage line changed:\n%s", firstLine(got))
	}
	if strings.Contains(prHelpText("tfac"), "triagefactory exec") {
		t.Error("applet `gh pr` help mentions 'triagefactory exec'")
	}
}

// TestResourceHelpTextsNameInvokedPrefix is the same contract for the two other
// banners in this package.
func TestResourceHelpTextsNameInvokedPrefix(t *testing.T) {
	for _, prefix := range prefixForms {
		if want, got := "Usage: "+prefix+" gh <resource> <action> [flags]\n", helpText(prefix); !strings.HasPrefix(got, want) {
			t.Errorf("helpText(%q) first line = %q, want %q", prefix, firstLine(got), want)
		}
		if want, got := "Usage: "+prefix+" gh actions <action> [flags]\n", actionsHelpText(prefix); !strings.HasPrefix(got, want) {
			t.Errorf("actionsHelpText(%q) first line = %q, want %q", prefix, firstLine(got), want)
		}
	}
}

// TestLiveHelpRoutesUseResolvedPrefix pins the wiring rather than the rule: the
// help routes an agent actually reaches must print the prefix resolved from
// argv0. Under `go test` that's the test binary's own path, so an output still
// naming `triagefactory exec` would mean a site kept its literal.
func TestLiveHelpRoutesUseResolvedPrefix(t *testing.T) {
	cases := []struct {
		name string
		run  func()
	}{
		{"gh", func() { Handle(context.Background(), nil, []string{"--help"}) }},
		{"gh pr", func() { handlePR(context.Background(), nil, []string{"--help"}) }},
		{"gh pr <action>", func() { handlePR(context.Background(), nil, []string{"start-review", "--help"}) }},
		{"gh actions", func() { handleActions(context.Background(), nil, []string{"--help"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, tc.run)
			if !strings.Contains(out, prog.Prefix()) {
				t.Errorf("help does not name the resolved prefix %q; got:\n%s", prog.Prefix(), out)
			}
			if strings.Contains(out, "triagefactory exec") {
				t.Errorf("help hardcodes 'triagefactory exec'; got:\n%s", out)
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
}
