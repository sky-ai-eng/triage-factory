package delegate

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestCapturedStateManifestJSONRoundTrip pins the child/parent control shape:
// metadata and staged member paths cross JSON, while the large byte fields do
// not.
func TestCapturedStateManifestJSONRoundTrip(t *testing.T) {
	in := worktree.CapturedState{
		Delta:          &worktree.GitDelta{Branch: "aa/feature", Head: "7a0f4f0c8b5dfb56564a3a9c861c4e9f6d68e690"},
		SessionID:      "sess",
		BundlePath:     "/tmp/capture/repo.bundle",
		PatchPath:      "/tmp/capture/uncommitted.patch",
		TranscriptPath: "/tmp/capture/session.jsonl",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("transcript\"")) {
		t.Fatalf("manifest contains a buffered transcript field: %s", b)
	}
	var out worktree.CapturedState
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Delta == nil || out.Delta.Branch != in.Delta.Branch || out.Delta.Head != in.Delta.Head {
		t.Errorf("git metadata changed: got %+v want %+v", out.Delta, in.Delta)
	}
	if out.BundlePath != in.BundlePath || out.PatchPath != in.PatchPath || out.TranscriptPath != in.TranscriptPath || out.SessionID != in.SessionID {
		t.Errorf("manifest paths changed: got %+v want %+v", out, in)
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
