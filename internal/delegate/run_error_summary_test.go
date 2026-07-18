package delegate

import "testing"

// TestErrorResultSummary pins the invariant that closes the failed-with-no-reason
// gap: an agent error result always yields a non-empty, human-readable summary —
// the runtime's own text when present, else a subtype-derived or generic
// stand-in. An empty return would let the frontend (which gates its failure
// detail on a non-empty summary) render a blank failure card again.
func TestErrorResultSummary(t *testing.T) {
	cases := []struct {
		name    string
		result  string
		subtype string
		want    string
	}{
		{
			name:    "prefers the runtime error text",
			result:  "No conversation found with session ID: aff66c16",
			subtype: "error_during_execution",
			want:    "No conversation found with session ID: aff66c16",
		},
		{
			name:   "trims surrounding whitespace",
			result: "  boom\n",
			want:   "boom",
		},
		{
			name:    "falls back to subtype when the text is blank",
			result:  "   ",
			subtype: "error_max_turns",
			want:    "agent runtime error: error_max_turns",
		},
		{
			name: "falls back to a generic stand-in when both are empty",
			want: "agent runtime reported an error result with no detail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errorResultSummary(tc.result, tc.subtype)
			if got != tc.want {
				t.Errorf("errorResultSummary(%q, %q) = %q, want %q", tc.result, tc.subtype, got, tc.want)
			}
			if got == "" {
				t.Error("errorResultSummary returned empty; a failed run must always carry a reason")
			}
		})
	}
}
