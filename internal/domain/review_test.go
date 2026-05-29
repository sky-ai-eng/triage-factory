package domain

import (
	"strings"
	"testing"
)

func TestNormalizeSeverity(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"blocker", SeverityBlocker, false},
		{"Major", SeverityMajor, false},
		{"  minor  ", SeverityMinor, false},
		{"CLEAN", SeverityClean, false},
		{"p2", "", true},
		{"critical", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeSeverity(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeSeverity(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeSeverity(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeSeverity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeSeverityErrorListsValidValues(t *testing.T) {
	_, err := NormalizeSeverity("nope")
	if err == nil {
		t.Fatal("expected error")
	}
	// The error must teach the caller the valid set so a third-party
	// review skill can self-correct.
	for _, lvl := range ValidSeverities {
		if !strings.Contains(err.Error(), lvl) {
			t.Errorf("error %q does not mention valid level %q", err.Error(), lvl)
		}
	}
}

func TestSeverityBadgeMarkdown(t *testing.T) {
	// Empty severity → no badge, body posted unchanged.
	if got := SeverityBadgeMarkdown(""); got != "" {
		t.Errorf("empty severity should yield empty badge, got %q", got)
	}
	// Unknown (shouldn't happen post-Normalize, but be defensive) → empty.
	if got := SeverityBadgeMarkdown("P2"); got != "" {
		t.Errorf("unknown severity should yield empty badge, got %q", got)
	}
	got := SeverityBadgeMarkdown(SeverityMajor)
	if !strings.Contains(got, "img.shields.io/badge/MAJOR-orange") {
		t.Errorf("badge missing shields.io URL with color: %q", got)
	}
	if !strings.HasPrefix(got, "<sub><sub>") {
		t.Errorf("badge should be wrapped in <sub> tags to render small: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("badge should end with a blank line so it sits above the body: %q", got)
	}
}
