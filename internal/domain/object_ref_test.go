package domain

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestObjectRef pins the object id's wire spelling: it round-trips, its JSON
// carries none of the characters a raw id has that a URL path segment cannot,
// an empty ref marshals empty, and a non-canonical or non-id spelling is
// refused.
func TestObjectRef(t *testing.T) {
	for _, raw := range []string{
		"branch|9b6c3c8e-1d0a-4f3e-9e2a-5a0b1c2d3e4f|refs/heads/feature/a+b",
		"pull_request|5b82978f-d06a-538a-8a0f-772a532727f3",
		"comment|https://github.com|github:comment:review:555",
	} {
		text, err := ObjectRef(raw).MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%q): %v", raw, err)
		}
		wire := string(text)
		if strings.ContainsAny(wire, "/|=+") {
			t.Errorf("wire spelling of %q = %q, want no '/', '|', '=' or '+'", raw, wire)
		}
		if got, ok := ParseObjectRef(wire); !ok || string(got) != raw {
			t.Errorf("ParseObjectRef(%q) = (%q, %v), want (%q, true)", wire, got, ok, raw)
		}
		type doc struct {
			ID ObjectRef `json:"id"`
		}
		js, err := json.Marshal(doc{ObjectRef(raw)})
		if err != nil || string(js) != `{"id":"`+wire+`"}` {
			t.Errorf("JSON of %q = %s (%v), want the wire spelling", raw, js, err)
		}
		var back doc
		if err := json.Unmarshal(js, &back); err != nil || string(back.ID) != raw {
			t.Errorf("JSON round-trip of %q = (%q, %v), want the raw id", raw, back.ID, err)
		}
	}
	if js, _ := json.Marshal(struct {
		ID ObjectRef `json:"id,omitempty"`
	}{}); string(js) != `{}` {
		t.Errorf("an empty ref with omitempty marshals %s, want {}", js)
	}

	// A 14-byte id leaves two zero bits in its last character. Bumping that
	// character sets one, which a lenient decoder still reads as the same id;
	// only the canonical spelling is accepted.
	text, _ := ObjectRef("pull_request|x").MarshalText()
	canonical := string(text)
	noncanonical := canonical[:len(canonical)-1] + string(canonical[len(canonical)-1]+1)
	if lenient, err := base64.RawURLEncoding.DecodeString(noncanonical); err != nil || string(lenient) != "pull_request|x" {
		t.Fatalf("fixture: a lenient decode of %q = (%q, %v), want the same id", noncanonical, lenient, err)
	}
	if _, ok := ParseObjectRef(noncanonical); ok {
		t.Errorf("ParseObjectRef accepted the non-canonical %q for %q", noncanonical, canonical)
	}

	padded := base64.URLEncoding.EncodeToString([]byte("pull_request|x"))
	unknownKind, _ := ObjectRef("widget|x").MarshalText()
	for _, bad := range []string{"", "pull_request|x", "%%%", padded, string(unknownKind)} {
		if _, ok := ParseObjectRef(bad); ok {
			t.Errorf("ParseObjectRef(%q) accepted it", bad)
		}
		var r ObjectRef
		if err := r.UnmarshalText([]byte(bad)); bad != "" && err == nil {
			t.Errorf("UnmarshalText(%q) accepted it", bad)
		}
	}
}

// TestIsObjectID_KnowsEveryKind ties IsObjectID to the ObjectKinds
// vocabulary, so a kind added to one is accepted by the other.
func TestIsObjectID_KnowsEveryKind(t *testing.T) {
	for _, kind := range ObjectKinds() {
		if !IsObjectID(kind + "|x") {
			t.Errorf("IsObjectID refuses kind %q", kind)
		}
	}
}
