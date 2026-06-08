package delegate

import "testing"

// TestClassifyAgentResult_Valid pins the valid-conclusion bucket: a recognized
// outcome carrying its required companion field (finish/continue + summary,
// abort + reason), including when wrapped in markdown fences or prose.
func TestClassifyAgentResult_Valid(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // expected outcome
	}{
		{"finish", `{"outcome":"finish","summary":"did the thing"}`, "finish"},
		{"continue", `{"outcome":"continue","summary":"handing off"}`, "continue"},
		{"abort with reason", `{"outcome":"abort","reason":"needs a human","summary":"looked into it"}`, "abort"},
		{"fenced", "```json\n{\"outcome\":\"finish\",\"summary\":\"ok\"}\n```", "finish"},
		{"prose-wrapped", "Here is my result:\n{\"outcome\":\"finish\",\"summary\":\"ok\"}", "finish"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, result := classifyAgentResult(tc.text)
			if class != turnValid {
				t.Fatalf("class = %v, want turnValid", class)
			}
			if result == nil || result.Outcome != tc.want {
				t.Errorf("result = %+v, want outcome %q", result, tc.want)
			}
		})
	}
}

// TestClassifyAgentResult_Invalid pins the invalid-attempt bucket: an envelope
// attempt (a JSON object carrying an `outcome`, or clearly envelope-shaped
// output that won't parse) that fails validation.
func TestClassifyAgentResult_Invalid(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"abort without reason", `{"outcome":"abort","summary":"stopped"}`},
		{"finish without summary", `{"outcome":"finish"}`},
		{"continue without summary", `{"outcome":"continue"}`},
		{"unrecognized outcome yield", `{"outcome":"yield"}`},
		{"unrecognized outcome other", `{"outcome":"frobnicate","summary":"x"}`},
		{"malformed JSON with outcome", `{"outcome":"finish","summary":}`},
		{"truncated envelope with outcome", `{"outcome":"finish","summary":"oops`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, result := classifyAgentResult(tc.text)
			if class != turnInvalid {
				t.Fatalf("class = %v, want turnInvalid", class)
			}
			if result != nil {
				t.Errorf("result = %+v, want nil for an invalid attempt", result)
			}
		})
	}
}

// TestClassifyAgentResult_None pins the open bucket: no envelope attempt at
// all — prose, nothing, an empty object, or unrelated JSON with no `outcome`.
func TestClassifyAgentResult_None(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"whitespace", "   \n  "},
		{"prose", "I finished reviewing the PR and left some comments."},
		{"prose with braces", "I'll set the config to {enabled: true} next."},
		{"empty object", `{}`},
		{"unrelated json object", `{"summary":"did stuff","foo":"bar"}`},
		{"unrelated json array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, result := classifyAgentResult(tc.text)
			if class != turnNone {
				t.Fatalf("class = %v, want turnNone", class)
			}
			if result != nil {
				t.Errorf("result = %+v, want nil for an open turn-end", result)
			}
		})
	}
}

// TestParseAgentResult_ValidOnly confirms parseAgentResult (the terminal-record
// helper) returns a result only for a valid conclusion — invalid attempts and
// open turn-ends come back nil.
func TestParseAgentResult_ValidOnly(t *testing.T) {
	if got := parseAgentResult(`{"outcome":"finish","summary":"done"}`); got == nil || got.Outcome != "finish" {
		t.Errorf("valid finish: got %+v", got)
	}
	if got := parseAgentResult(`{"outcome":"abort","summary":"stopped"}`); got != nil {
		t.Errorf("abort without reason should not parse as valid: got %+v", got)
	}
	if got := parseAgentResult(`just prose`); got != nil {
		t.Errorf("prose should not parse: got %+v", got)
	}
}
