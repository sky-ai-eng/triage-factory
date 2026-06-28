package github

import (
	"strings"
	"testing"
)

// intSetEqual compares two map[int]bool values for equality.
func intSetEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// --- parsePatchLines ---

func TestParsePatchLines_BasicPatch(t *testing.T) {
	// Added, deleted, and context lines — only added and context are commentable.
	patch := "@@ -1,4 +1,4 @@\n line1\n-old line\n+new line\n line3\n line4"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true, 2: true, 3: true, 4: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines basic = %v, want %v", got, want)
	}
}

func TestParsePatchLines_TrailingNewlineNotCounted(t *testing.T) {
	// A patch ending with \n produces a trailing empty string from strings.Split.
	// That empty string must NOT be recorded as a commentable line.
	patch := "@@ -1,2 +1,2 @@\n line1\n+added\n"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true, 2: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines trailing newline = %v, want %v", got, want)
	}
	if got[3] {
		t.Error("line 3 should not be commentable (trailing empty string from split)")
	}
}

func TestParsePatchLines_NoNewlineAtEndOfFile(t *testing.T) {
	// The "\ No newline at end of file" marker must not be counted as a line.
	patch := "@@ -1,1 +1,1 @@\n-old\n+new\n\\ No newline at end of file"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines no-newline marker = %v, want %v", got, want)
	}
}

func TestParsePatchLines_EmptyPatch(t *testing.T) {
	got := parsePatchLines("")
	if len(got) != 0 {
		t.Errorf("parsePatchLines empty = %v, want empty map", got)
	}
}

func TestParsePatchLines_OnlyDeletions(t *testing.T) {
	// All removed lines; new side has no content.
	patch := "@@ -1,3 +0,0 @@\n-line1\n-line2\n-line3"
	got := parsePatchLines(patch)
	if len(got) != 0 {
		t.Errorf("parsePatchLines only deletions = %v, want empty", got)
	}
}

func TestParsePatchLines_MultipleHunks(t *testing.T) {
	// Two hunks at different positions in the file.
	patch := "@@ -1,3 +1,3 @@\n ctx1\n-del1\n+add1\n@@ -10,3 +10,3 @@\n ctx10\n-del10\n+add10"
	got := parsePatchLines(patch)
	// Hunk 1: new-side lines 1 (ctx1), 2 (add1)
	// Hunk 2: new-side lines 10 (ctx10), 11 (add10)
	want := map[int]bool{1: true, 2: true, 10: true, 11: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines multiple hunks = %v, want %v", got, want)
	}
}

func TestParsePatchLines_OnlyAdditions(t *testing.T) {
	// New file — every line is an addition.
	patch := "@@ -0,0 +1,3 @@\n+line1\n+line2\n+line3"
	got := parsePatchLines(patch)
	want := map[int]bool{1: true, 2: true, 3: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines only additions = %v, want %v", got, want)
	}
}

func TestParsePatchLines_ContextOnly(t *testing.T) {
	// Pure context (unchanged) lines are still commentable.
	patch := "@@ -5,3 +5,3 @@\n line5\n line6\n line7"
	got := parsePatchLines(patch)
	want := map[int]bool{5: true, 6: true, 7: true}
	if !intSetEqual(got, want) {
		t.Errorf("parsePatchLines context only = %v, want %v", got, want)
	}
}

// --- DiffLinesFromPatches ---

func TestDiffLinesFromPatches_SingleFile(t *testing.T) {
	files := []PRFile{
		{Filename: "a.go", Patch: "@@ -1,2 +1,2 @@\n ctx\n+added\n"},
	}
	got := DiffLinesFromPatches(files)
	if !got["a.go"][1] || !got["a.go"][2] {
		t.Errorf("a.go: expected lines 1,2 commentable, got %v", got["a.go"])
	}
	if got["a.go"][3] {
		t.Error("a.go: line 3 should not be commentable (trailing newline)")
	}
}

func TestDiffLinesFromPatches_MultipleFiles(t *testing.T) {
	files := []PRFile{
		{Filename: "a.go", Patch: "@@ -1,1 +1,1 @@\n+lineA\n"},
		{Filename: "b.go", Patch: "@@ -1,1 +1,1 @@\n+lineB\n"},
	}
	got := DiffLinesFromPatches(files)
	if _, ok := got["a.go"]; !ok {
		t.Error("expected a.go in result")
	}
	if _, ok := got["b.go"]; !ok {
		t.Error("expected b.go in result")
	}
	if !got["a.go"][1] {
		t.Errorf("a.go line 1 should be commentable: %v", got["a.go"])
	}
	if !got["b.go"][1] {
		t.Errorf("b.go line 1 should be commentable: %v", got["b.go"])
	}
}

func TestDiffLinesFromPatches_BinaryFileNoPatch(t *testing.T) {
	// Binary files have no patch — should produce an empty (but present) entry.
	files := []PRFile{
		{Filename: "image.png", Patch: ""},
	}
	got := DiffLinesFromPatches(files)
	if _, ok := got["image.png"]; !ok {
		t.Error("expected image.png key to be present even with empty patch")
	}
	if len(got["image.png"]) != 0 {
		t.Errorf("expected no commentable lines for binary file, got %v", got["image.png"])
	}
}

func TestDiffLinesFromPatches_EmptyFileList(t *testing.T) {
	got := DiffLinesFromPatches(nil)
	if len(got) != 0 {
		t.Errorf("expected empty result for nil files, got %v", got)
	}
}

// --- DiffLines ---

func TestDiffLines_BasicDiff(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,4 +1,4 @@\n context\n-old\n+new\n line3\n line4"
	got := DiffLines(diff)
	want := map[int]bool{1: true, 2: true, 3: true, 4: true}
	if !intSetEqual(got["foo.go"], want) {
		t.Errorf("DiffLines basic = %v, want %v", got["foo.go"], want)
	}
}

func TestDiffLines_TrailingNewlineNotCounted(t *testing.T) {
	// A diff ending with \n must not add a spurious line via the trailing empty string.
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n+new\n"
	got := DiffLines(diff)
	want := map[int]bool{1: true, 2: true}
	if !intSetEqual(got["foo.go"], want) {
		t.Errorf("DiffLines trailing newline = %v, want %v", got["foo.go"], want)
	}
	if got["foo.go"][3] {
		t.Error("line 3 should not be commentable (trailing empty string from split)")
	}
}

func TestDiffLines_MultipleFiles(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n@@ -1,1 +1,1 @@\n+lineA\ndiff --git a/b.go b/b.go\n@@ -1,1 +1,1 @@\n+lineB"
	got := DiffLines(diff)
	if _, ok := got["a.go"]; !ok {
		t.Error("expected a.go in result")
	}
	if _, ok := got["b.go"]; !ok {
		t.Error("expected b.go in result")
	}
	if !got["a.go"][1] {
		t.Errorf("a.go line 1 should be commentable: %v", got["a.go"])
	}
	if !got["b.go"][1] {
		t.Errorf("b.go line 1 should be commentable: %v", got["b.go"])
	}
}

func TestDiffLines_EmptyDiff(t *testing.T) {
	got := DiffLines("")
	if len(got) != 0 {
		t.Errorf("DiffLines empty = %v, want empty map", got)
	}
}

func TestDiffLines_CRLF(t *testing.T) {
	// A CRLF diff must key the file cleanly ("foo.go", not "foo.go\r").
	diff := "diff --git a/foo.go b/foo.go\r\n@@ -1,4 +1,4 @@\r\n context\r\n-old\r\n+new\r\n line3\r\n line4\r\n"
	got := DiffLines(diff)
	if _, ok := got["foo.go"]; !ok {
		t.Fatalf("expected clean key \"foo.go\", got keys %v", keysOf(got))
	}
	want := map[int]bool{1: true, 2: true, 3: true, 4: true}
	if !intSetEqual(got["foo.go"], want) {
		t.Errorf("DiffLines CRLF = %v, want %v", got["foo.go"], want)
	}
}

func keysOf(m map[string]map[int]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- DiffHunks ---

func hunksEqual(a, b []Hunk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiffHunks_SingleHunk(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,4 +1,4 @@\n ctx\n-old\n+new\n line3\n line4"
	got := DiffHunks(diff)
	want := []Hunk{{NewStart: 1, NewEnd: 4}}
	if !hunksEqual(got["foo.go"], want) {
		t.Errorf("DiffHunks single = %v, want %v", got["foo.go"], want)
	}
}

func TestDiffHunks_MultipleHunksSameFile(t *testing.T) {
	// Two hunks in one file — the gap between them is what ValidateCommentRange
	// uses to reject cross-hunk multi-line comments.
	diff := "diff --git a/foo.go b/foo.go\n" +
		"@@ -1,3 +1,3 @@\n ctx1\n-del1\n+add1\n" +
		"@@ -10,3 +10,3 @@\n ctx10\n-del10\n+add10"
	got := DiffHunks(diff)
	want := []Hunk{{NewStart: 1, NewEnd: 2}, {NewStart: 10, NewEnd: 11}}
	if !hunksEqual(got["foo.go"], want) {
		t.Errorf("DiffHunks multi-hunk = %v, want %v", got["foo.go"], want)
	}
}

func TestDiffHunks_MultipleFiles(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n@@ -1,1 +1,2 @@\n+added\n ctx\n" +
		"diff --git a/b.go b/b.go\n@@ -5,1 +5,2 @@\n ctx\n+added"
	got := DiffHunks(diff)
	wantA := []Hunk{{NewStart: 1, NewEnd: 2}}
	wantB := []Hunk{{NewStart: 5, NewEnd: 6}}
	if !hunksEqual(got["a.go"], wantA) {
		t.Errorf("a.go hunks = %v, want %v", got["a.go"], wantA)
	}
	if !hunksEqual(got["b.go"], wantB) {
		t.Errorf("b.go hunks = %v, want %v", got["b.go"], wantB)
	}
}

func TestDiffHunks_DeletionOnlyHunkProducesNoHunk(t *testing.T) {
	// Pure-deletion hunk has no commentable new-side line, so it doesn't
	// produce a Hunk entry (NewEnd would be < NewStart).
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,3 +0,0 @@\n-line1\n-line2\n-line3"
	got := DiffHunks(diff)
	// File is still present in the map (registered when the diff --git header
	// was seen) but has no hunks.
	if hunks := got["foo.go"]; len(hunks) != 0 {
		t.Errorf("DiffHunks deletion-only = %v, want no hunks", hunks)
	}
}

func TestDiffHunks_AdditionOnlyAtEOF(t *testing.T) {
	// New file: every line is a new-side addition starting at line 1.
	diff := "diff --git a/new.go b/new.go\n@@ -0,0 +1,3 @@\n+line1\n+line2\n+line3"
	got := DiffHunks(diff)
	want := []Hunk{{NewStart: 1, NewEnd: 3}}
	if !hunksEqual(got["new.go"], want) {
		t.Errorf("DiffHunks addition-only = %v, want %v", got["new.go"], want)
	}
}

func TestDiffHunks_TrailingNewlineDoesNotInflateRange(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n ctx\n+new\n"
	got := DiffHunks(diff)
	want := []Hunk{{NewStart: 1, NewEnd: 2}}
	if !hunksEqual(got["foo.go"], want) {
		t.Errorf("DiffHunks trailing newline = %v, want %v", got["foo.go"], want)
	}
}

func TestDiffHunks_EmptyDiff(t *testing.T) {
	if got := DiffHunks(""); len(got) != 0 {
		t.Errorf("DiffHunks empty = %v, want empty map", got)
	}
}

func TestDiffHunks_CRLF(t *testing.T) {
	// A CRLF diff must key the file cleanly so ValidateCommentRange resolves it.
	diff := "diff --git a/foo.go b/foo.go\r\n@@ -1,4 +1,4 @@\r\n ctx\r\n-old\r\n+new\r\n line3\r\n line4\r\n"
	got := DiffHunks(diff)
	want := []Hunk{{NewStart: 1, NewEnd: 4}}
	if !hunksEqual(got["foo.go"], want) {
		t.Errorf("DiffHunks CRLF = %v, want %v (keys: %v)", got["foo.go"], want, got)
	}
	// The clean key means the downstream validator finds the file.
	if msg := ValidateCommentRange(got, "foo.go", 3, nil); msg != "" {
		t.Errorf("ValidateCommentRange on CRLF diff = %q, want valid", msg)
	}
}

// --- DiffHunksFromPatches ---

func TestDiffHunksFromPatches_MultiHunk(t *testing.T) {
	files := []PRFile{{
		Filename: "a.go",
		Patch: "@@ -1,3 +1,3 @@\n ctx\n-del\n+add\n" +
			"@@ -20,2 +20,3 @@\n ctx20\n+added\n more",
	}}
	got := DiffHunksFromPatches(files)
	want := []Hunk{{NewStart: 1, NewEnd: 2}, {NewStart: 20, NewEnd: 22}}
	if !hunksEqual(got["a.go"], want) {
		t.Errorf("DiffHunksFromPatches multi-hunk = %v, want %v", got["a.go"], want)
	}
}

func TestDiffHunksFromPatches_BinaryFileNoPatch(t *testing.T) {
	files := []PRFile{{Filename: "image.png", Patch: ""}}
	got := DiffHunksFromPatches(files)
	if _, ok := got["image.png"]; !ok {
		t.Error("expected image.png key present even with empty patch")
	}
	if len(got["image.png"]) != 0 {
		t.Errorf("expected no hunks for binary file, got %v", got["image.png"])
	}
}

// --- ValidateCommentRange ---

func intPtr(i int) *int { return &i }

func TestValidateCommentRange_SingleLineValid(t *testing.T) {
	hunks := map[string][]Hunk{"a.go": {{NewStart: 1, NewEnd: 5}}}
	if msg := ValidateCommentRange(hunks, "a.go", 3, nil); msg != "" {
		t.Errorf("expected valid, got error: %q", msg)
	}
}

func TestValidateCommentRange_MultiLineInSameHunk(t *testing.T) {
	hunks := map[string][]Hunk{"a.go": {{NewStart: 10, NewEnd: 30}}}
	if msg := ValidateCommentRange(hunks, "a.go", 25, intPtr(15)); msg != "" {
		t.Errorf("expected valid, got error: %q", msg)
	}
}

func TestValidateCommentRange_CrossHunkRejected(t *testing.T) {
	// start_line in hunk A, line in hunk B — this is the exact 422 we're
	// trying to prevent at submit time.
	hunks := map[string][]Hunk{"a.go": {
		{NewStart: 1, NewEnd: 5},
		{NewStart: 20, NewEnd: 30},
	}}
	msg := ValidateCommentRange(hunks, "a.go", 25, intPtr(3))
	if msg == "" {
		t.Fatal("expected cross-hunk error, got nil")
	}
	if !strings.Contains(msg, "same diff hunk") {
		t.Errorf("error should mention same hunk requirement: %q", msg)
	}
	// The message must list the hunks so the agent can pick a valid range
	// on retry without another round-trip.
	if !strings.Contains(msg, "[1–5]") || !strings.Contains(msg, "[20–30]") {
		t.Errorf("error should include hunk list, got: %q", msg)
	}
}

func TestValidateCommentRange_LineNotInDiff(t *testing.T) {
	hunks := map[string][]Hunk{"a.go": {{NewStart: 1, NewEnd: 5}, {NewStart: 30, NewEnd: 40}}}
	msg := ValidateCommentRange(hunks, "a.go", 100, nil)
	if !strings.Contains(msg, "line 100") || !strings.Contains(msg, "not part of the diff") {
		t.Errorf("expected line-not-in-diff error, got: %q", msg)
	}
	// The error must include the file's hunks so the agent can pick a
	// valid line on retry without another round-trip — same pattern as
	// the cross-hunk and start_line-not-in-diff errors.
	if !strings.Contains(msg, "[1–5]") || !strings.Contains(msg, "[30–40]") {
		t.Errorf("error should include hunk list, got: %q", msg)
	}
}

func TestValidateCommentRange_StartLineNotInDiff(t *testing.T) {
	// `line` falls in a hunk but `start_line` doesn't — distinct error
	// from the cross-hunk case.
	hunks := map[string][]Hunk{"a.go": {{NewStart: 10, NewEnd: 20}}}
	msg := ValidateCommentRange(hunks, "a.go", 15, intPtr(5))
	if !strings.Contains(msg, "start_line 5") || !strings.Contains(msg, "not part of the diff") {
		t.Errorf("expected start_line-not-in-diff error, got: %q", msg)
	}
}

func TestValidateCommentRange_StartLineGreaterThanLine(t *testing.T) {
	hunks := map[string][]Hunk{"a.go": {{NewStart: 1, NewEnd: 100}}}
	msg := ValidateCommentRange(hunks, "a.go", 5, intPtr(10))
	if !strings.Contains(msg, "start_line 10") || !strings.Contains(msg, "≤ line 5") {
		t.Errorf("expected start_line>line error, got: %q", msg)
	}
}

func TestValidateCommentRange_FileNotInDiff(t *testing.T) {
	hunks := map[string][]Hunk{"a.go": {{NewStart: 1, NewEnd: 5}}}
	msg := ValidateCommentRange(hunks, "missing.go", 1, nil)
	if !strings.Contains(msg, "missing.go") || !strings.Contains(msg, "not in the diff") {
		t.Errorf("expected file-not-in-diff error, got: %q", msg)
	}
}

func TestValidateCommentRange_BoundaryLines(t *testing.T) {
	// Both endpoints of a hunk must be valid (inclusive bounds).
	hunks := map[string][]Hunk{"a.go": {{NewStart: 10, NewEnd: 20}}}
	if msg := ValidateCommentRange(hunks, "a.go", 10, intPtr(10)); msg != "" {
		t.Errorf("expected valid at NewStart, got: %q", msg)
	}
	if msg := ValidateCommentRange(hunks, "a.go", 20, intPtr(10)); msg != "" {
		t.Errorf("expected valid full-hunk range, got: %q", msg)
	}
	if msg := ValidateCommentRange(hunks, "a.go", 21, nil); msg == "" {
		t.Error("expected error one past NewEnd")
	}
	if msg := ValidateCommentRange(hunks, "a.go", 9, nil); msg == "" {
		t.Error("expected error one before NewStart")
	}
}
