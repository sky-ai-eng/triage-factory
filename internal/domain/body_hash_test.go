package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONBodyHash(t *testing.T) {
	for _, raw := range []string{"", "{", "false", "[]", "17"} {
		if got := JSONBodyHash(json.RawMessage(raw)); got != "" {
			t.Fatalf("unknown body %q has hash %q", raw, got)
		}
	}
	for _, pair := range [][2]string{
		{`null`, `""`},
		{`"a\u0026b"`, `"a&b"`},
		{`{"type":"doc","version":1}`, ` { "version": 1, "type": "doc" } `},
	} {
		a, b := JSONBodyHash([]byte(pair[0])), JSONBodyHash([]byte(pair[1]))
		if len(a) != 64 || a != b {
			t.Errorf("equivalent bodies must have equal, nonempty SHA256: %q, %q", a, b)
		}
	}
	a, _ := json.Marshal(strings.Repeat("é", 3000) + "before")
	b, _ := json.Marshal(strings.Repeat("é", 3000) + "after")
	if JSONBodyHash(a) == JSONBodyHash(b) {
		t.Fatal("edit beyond the preview cap was lost")
	}
	// ADF links matter even when their displayed text is unchanged.
	if JSONBodyHash([]byte(`{"text":"spec","marks":[{"attrs":{"href":"/a"}}]}`)) ==
		JSONBodyHash([]byte(`{"text":"spec","marks":[{"attrs":{"href":"/b"}}]}`)) {
		t.Fatal("ADF link edit was lost")
	}
}
