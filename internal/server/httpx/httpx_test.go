package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSON_RejectsTrailingData pins the contract that DecodeJSON
// accepts exactly one top-level JSON value (optionally followed by
// whitespace) and rejects anything trailing it with a 400. The trailing
// `}` / `]` cases are the ones dec.More() alone misses, since More()
// reports false at the close of the current array/object.
func TestDecodeJSON_RejectsTrailingData(t *testing.T) {
	type payload struct {
		A int `json:"a"`
	}
	cases := []struct {
		name       string
		body       string
		wantAccept bool
	}{
		{"clean object", `{"a":1}`, true},
		{"trailing whitespace", "{\"a\":1}  \n ", true},
		{"second object", `{"a":1} {"extra":true}`, false},
		{"trailing close brace", `{"a":1}}`, false},
		{"trailing close bracket", `{"a":1}]`, false},
		{"trailing number", `{"a":1}9`, false},
		{"trailing letters", `{"a":1}garbage`, false},
		{"malformed", `{"a":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var v payload
			got := DecodeJSON(rec, req, &v, "")
			if got != tc.wantAccept {
				t.Errorf("DecodeJSON(%q) accepted=%v, want %v (status=%d)", tc.body, got, tc.wantAccept, rec.Code)
			}
			if !tc.wantAccept && rec.Code != 400 {
				t.Errorf("DecodeJSON(%q) rejected but status=%d, want 400", tc.body, rec.Code)
			}
		})
	}
}
