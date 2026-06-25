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
	// Color is the Tailwind orange-500 hex (matches the in-app chip), not
	// shields' named "orange".
	if !strings.Contains(got, "img.shields.io/badge/MAJOR-f97316") {
		t.Errorf("badge missing shields.io URL with color: %q", got)
	}
	if !strings.HasPrefix(got, "<sub><sub>") {
		t.Errorf("badge should be wrapped in <sub> tags to render small: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("badge should end with a blank line so it sits above the body: %q", got)
	}
}

func TestParseSeverityBadge_RoundTrip(t *testing.T) {
	// ParseSeverityBadge is the exact inverse of SeverityBadgeMarkdown: badge a
	// body, then parse it back, and we must recover both the level and the
	// original clean body byte-for-byte.
	for _, sev := range ValidSeverities {
		body := "This is the finding.\nSecond line."
		badged := SeverityBadgeMarkdown(sev) + body
		gotSev, gotBody := ParseSeverityBadge(badged)
		if gotSev != sev {
			t.Errorf("ParseSeverityBadge severity = %q, want %q", gotSev, sev)
		}
		if gotBody != body {
			t.Errorf("ParseSeverityBadge body = %q, want %q", gotBody, body)
		}
	}
}

func TestParseSeverityBadge_NoBadge(t *testing.T) {
	// A body with no badge round-trips untouched (severity "", body verbatim) —
	// a comment authored without --severity, or a human-written comment.
	body := "plain body without a badge"
	gotSev, gotBody := ParseSeverityBadge(body)
	if gotSev != "" {
		t.Errorf("ParseSeverityBadge severity = %q, want empty", gotSev)
	}
	if gotBody != body {
		t.Errorf("ParseSeverityBadge body = %q, want %q", gotBody, body)
	}
}

func TestParseSeverityBadge_EmptyBody(t *testing.T) {
	if sev, body := ParseSeverityBadge(""); sev != "" || body != "" {
		t.Errorf("ParseSeverityBadge(\"\") = (%q, %q), want empty", sev, body)
	}
}
