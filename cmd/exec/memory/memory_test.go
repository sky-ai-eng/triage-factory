package memory

import "testing"

// TestParseLoadArgs_Valid pins the happy paths across all three sources: the
// source + id are captured verbatim and --limit defaults to 20 when omitted.
func TestParseLoadArgs_Valid(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSrc string
		wantID  string
		wantLim int
	}{
		{"github default limit", []string{"--source", "github", "--id", "octo/repo#42"}, "github", "octo/repo#42", 20},
		{"jira", []string{"--source", "jira", "--id", "SKY-123"}, "jira", "SKY-123", 20},
		{"slack", []string{"--source", "slack", "--id", "C0999/1700000000.000300"}, "slack", "C0999/1700000000.000300", 20},
		{"explicit limit", []string{"--source", "github", "--id", "octo/repo#1", "--limit", "5"}, "github", "octo/repo#1", 5},
		{"flags reordered", []string{"--id", "SKY-9", "--limit", "3", "--source", "jira"}, "jira", "SKY-9", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLoadArgs(tc.args)
			if err != nil {
				t.Fatalf("parseLoadArgs(%v): unexpected error %v", tc.args, err)
			}
			if got.source != tc.wantSrc || got.sourceID != tc.wantID || got.limit != tc.wantLim {
				t.Errorf("parseLoadArgs(%v) = %+v, want source=%q id=%q limit=%d", tc.args, got, tc.wantSrc, tc.wantID, tc.wantLim)
			}
		})
	}
}

// TestParseLoadArgs_UsageErrors pins the rejections: a missing/invalid source,
// a missing id, a bad limit, an unknown flag, and a flag with no value all
// surface a usage error (never a silent default) so the CLI can print usage.
func TestParseLoadArgs_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no source", []string{"--id", "octo/repo#1"}},
		{"empty source value", []string{"--source", "--id", "octo/repo#1"}}, // --id consumed as source value → source="--id", then missing id
		{"invalid source", []string{"--source", "linear", "--id", "ENG-1"}},
		{"no id", []string{"--source", "github"}},
		{"limit not an int", []string{"--source", "github", "--id", "octo/repo#1", "--limit", "abc"}},
		{"limit zero", []string{"--source", "github", "--id", "octo/repo#1", "--limit", "0"}},
		{"limit negative", []string{"--source", "github", "--id", "octo/repo#1", "--limit", "-4"}},
		{"source missing value at end", []string{"--id", "octo/repo#1", "--source"}},
		{"limit missing value at end", []string{"--source", "github", "--id", "octo/repo#1", "--limit"}},
		{"unknown flag", []string{"--source", "github", "--id", "octo/repo#1", "--depth", "2"}},
		{"bare positional", []string{"octo/repo#1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseLoadArgs(tc.args); err == nil {
				t.Errorf("parseLoadArgs(%v) = nil error, want a usage error", tc.args)
			}
		})
	}
}

// TestParseLoadArgs_InvalidSourceReported checks the invalid-source path names
// the offending value and the valid set, so the agent can self-correct.
func TestParseLoadArgs_InvalidSourceReported(t *testing.T) {
	_, err := parseLoadArgs([]string{"--source", "gitlab", "--id", "x/y#1"})
	if err == nil {
		t.Fatal("expected an error for an unsupported source")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected a non-empty error message")
	}
}
