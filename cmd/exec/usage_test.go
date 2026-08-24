package exec

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// TestHelpTextUsageLine golden-pins the top-level usage line under each invoked
// form. The canonical one is byte-identical to what the binary has always
// printed; the applet names `tfac` without the implicit `exec` word — printing
// that word under the applet is what taught the doubled spelling.
func TestHelpTextUsageLine(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
	}{
		{"triagefactory exec", "Usage: triagefactory exec <command> [args]\n"},
		{"tfac", "Usage: tfac <command> [args]\n"},
		{"./triagefactory exec", "Usage: ./triagefactory exec <command> [args]\n"},
	}
	for _, tt := range tests {
		if got := helpText(tt.prefix, nil); !strings.HasPrefix(got, tt.want) {
			t.Errorf("helpText(%q) first line = %q, want %q", tt.prefix, firstLine(got), tt.want)
		}
	}

	// Everything past the usage line is the shared verb documentation, which
	// names its verbs bare — so the prefix appears in the usage line alone and
	// the rest of the output is invariant.
	canonical := strings.TrimPrefix(helpText("triagefactory exec", nil), tests[0].want)
	applet := strings.TrimPrefix(helpText("tfac", nil), tests[1].want)
	if canonical != applet {
		t.Error("help bodies diverge between invoked forms; only the usage line may differ")
	}
	if strings.Contains(applet, "triagefactory exec") {
		t.Error("applet help mentions 'triagefactory exec'; it teaches a prefix that is implicit under tfac")
	}
}

// TestUnknownCommandText pins the loser path's wording: an unknown verb reports
// the form the caller actually typed, so the hint is copy-pasteable.
func TestUnknownCommandText(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
	}{
		{"triagefactory exec", "unknown exec command: bogus\nRun 'triagefactory exec --help' for usage.\n"},
		{"tfac", "unknown exec command: bogus\nRun 'tfac --help' for usage.\n"},
		{"/usr/local/bin/triagefactory exec", "unknown exec command: bogus\nRun '/usr/local/bin/triagefactory exec --help' for usage.\n"},
	}
	for _, tt := range tests {
		if got := unknownCommandText(tt.prefix, "bogus"); got != tt.want {
			t.Errorf("unknownCommandText(%q, \"bogus\") = %q, want %q", tt.prefix, got, tt.want)
		}
	}
}

// TestHelpIsWiredToArgv0 pins that the live help route reads the resolved
// prefix rather than any baked-in name: under `go test` argv0 is the test
// binary, so its own path is what must appear.
func TestHelpIsWiredToArgv0(t *testing.T) {
	out := helpText(prog.Prefix(), nil)
	if !strings.HasPrefix(out, "Usage: "+prog.Prefix()+" <command> [args]") {
		t.Errorf("usage line does not name the invoked prefix %q:\n%s", prog.Prefix(), firstLine(out))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
}
