package delegate

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestGitDeltaJSONRoundTrip pins the serialization boundary between the
// snapshot-capture child (which emits the delta as JSON) and captureIsolated
// (which decodes it): the head/branch strings and the binary bundle/patch must
// survive verbatim. encoding/json base64-encodes []byte, so this is what keeps
// a bundle or patch from being corrupted across the re-exec.
func TestGitDeltaJSONRoundTrip(t *testing.T) {
	in := &worktree.GitDelta{
		Branch: "aa/feature",
		Head:   "7a0f4f0c8b5dfb56564a3a9c861c4e9f6d68e690",
		Bundle: []byte{0x00, 0x01, 0xff, 0xfe, 'g', 'i', 't', 0x00},
		Patch:  []byte("diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n"),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out worktree.GitDelta
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Branch != in.Branch || out.Head != in.Head {
		t.Errorf("strings changed: got {%q %q} want {%q %q}", out.Branch, out.Head, in.Branch, in.Head)
	}
	if !bytes.Equal(out.Bundle, in.Bundle) {
		t.Errorf("bundle bytes changed: got %v want %v", out.Bundle, in.Bundle)
	}
	if !bytes.Equal(out.Patch, in.Patch) {
		t.Errorf("patch bytes changed")
	}
}

// TestGitDeltaNilJSON pins that a nil delta (a non-git run root) marshals to
// "null" — how it rides inside the CapturedState envelope's `delta` field, which
// captureIsolated then decodes back to a nil delta.
func TestGitDeltaNilJSON(t *testing.T) {
	b, err := json.Marshal((*worktree.GitDelta)(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Errorf("nil delta marshaled to %q, want null", b)
	}
}
