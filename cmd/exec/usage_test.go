package exec

import (
	"strings"
	"testing"
)

// TestUsageName pins the argv0 → printed-name mapping. Only the applet's own
// basename switches the spelling; anything else (including a path that merely
// contains it) keeps the canonical form.
func TestUsageName(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{"tfac", "tfac"},
		{"/opt/tf/bin/tfac", "tfac"},
		{"triagefactory", "triagefactory exec"},
		{"/usr/local/bin/triagefactory", "triagefactory exec"},
		{"/opt/tfac/bin/triagefactory", "triagefactory exec"},
		{"", "triagefactory exec"},
	}
	for _, tt := range tests {
		if got := usageName(tt.argv0); got != tt.want {
			t.Errorf("usageName(%q) = %q, want %q", tt.argv0, got, tt.want)
		}
	}
}

// TestHelpTextUsageLine golden-pins both usage lines: the canonical one is
// byte-identical to what the binary has always printed, and the applet one
// names `tfac` without the implicit `exec` word — printing that word under the
// applet is what taught the doubled spelling in the first place.
func TestHelpTextUsageLine(t *testing.T) {
	const (
		canonicalLine = "Usage: triagefactory exec <command> [args]\n"
		appletLine    = "Usage: tfac <command> [args]\n"
	)
	canonical := helpText(canonicalName)
	if !strings.HasPrefix(canonical, canonicalLine) {
		t.Errorf("canonical help first line = %q, want %q", firstLine(canonical), canonicalLine)
	}
	applet := helpText(AppletName)
	if !strings.HasPrefix(applet, appletLine) {
		t.Errorf("applet help first line = %q, want %q", firstLine(applet), appletLine)
	}
	if strings.Contains(applet, "triagefactory exec") {
		t.Error("applet help mentions 'triagefactory exec'; it teaches a prefix that is implicit under tfac")
	}
	// Everything past the usage line is the shared verb documentation, so the
	// canonical output is unchanged apart from the name.
	if body := strings.TrimPrefix(canonical, canonicalLine); body != strings.TrimPrefix(applet, appletLine) {
		t.Error("help bodies diverge between the two names; only the usage line may differ")
	}
}

// TestUnknownCommandText pins the loser path's wording under both names: an
// unknown verb reports the form the caller actually typed.
func TestUnknownCommandText(t *testing.T) {
	if got, want := unknownCommandText(canonicalName, "bogus"),
		"unknown exec command: bogus\nRun 'triagefactory exec --help' for usage.\n"; got != want {
		t.Errorf("canonical = %q, want %q", got, want)
	}
	if got, want := unknownCommandText(AppletName, "bogus"),
		"unknown exec command: bogus\nRun 'tfac --help' for usage.\n"; got != want {
		t.Errorf("applet = %q, want %q", got, want)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
}
