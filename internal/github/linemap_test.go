package github

import "testing"

// --- parseHunkOldStart ---

func TestParseHunkOldStart(t *testing.T) {
	cases := []struct {
		header string
		want   int
	}{
		{"@@ -1,4 +1,4 @@", 1},
		{"@@ -10,7 +12,8 @@", 10},
		{"@@ -0,0 +1,3 @@", 0},                // new file
		{"@@ -1,3 +0,0 @@", 1},                // deleted file
		{"@@ -5 +5 @@", 5},                    // counts omitted (== 1)
		{"@@ -42,6 +42,6 @@ func foo()", 42},  // with section text
		{"@@ -1,3 +1,3 @@ -deprecated_fn", 1}, // section text starting with a hyphen
		{"@@ -2,0 +3,1 @@", 2},                // pure-insertion hunk (oldLen 0)
		{"no header here", 0},
	}
	for _, c := range cases {
		if got := parseHunkOldStart(c.header); got != c.want {
			t.Errorf("parseHunkOldStart(%q) = %d, want %d", c.header, got, c.want)
		}
	}
}

// --- MapComment: single-line, table-driven over synthetic diffs ---

func TestMapComment_SingleLine(t *testing.T) {
	cases := []struct {
		name string
		diff string
		path string
		line int
		want LineMapResult
	}{
		{
			// Insert a line at the top → the anchored line shifts down by one.
			name: "insertion above → moved",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,4 +1,5 @@\n+X\n a\n b\n c\n d\n",
			path: "f.go",
			line: 3, // "c"
			want: LineMapResult{Status: LineMapMoved, Line: 4},
		},
		{
			// The anchored line itself is edited (a '-' old line + a '+' new line).
			name: "edit on the line → outdated",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,4 +1,4 @@\n a\n b\n-c\n+c2\n d\n",
			path: "f.go",
			line: 3, // "c"
			want: LineMapResult{Status: LineMapOutdated},
		},
		{
			// The anchored line is removed outright.
			name: "deletion of the line → outdated",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,4 +1,3 @@\n a\n b\n-c\n d\n",
			path: "f.go",
			line: 3, // "c"
			want: LineMapResult{Status: LineMapOutdated},
		},
		{
			// Change at the bottom of a long file; the anchor at the top is
			// outside every hunk and does not move.
			name: "unrelated change elsewhere → unchanged",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -7,4 +7,4 @@\n l7\n l8\n l9\n-l10\n+L10\n",
			path: "f.go",
			line: 1,
			want: LineMapResult{Status: LineMapUnchanged, Line: 1},
		},
		{
			// Insertion strictly below the anchor (far enough that the anchor is
			// outside the hunk) leaves its number unchanged.
			name: "insertion below → unchanged",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -6,5 +6,6 @@\n l6\n l7\n l8\n+X\n l9\n l10\n",
			path: "f.go",
			line: 2,
			want: LineMapResult{Status: LineMapUnchanged, Line: 2},
		},
		{
			// Deletion above shifts the (later) anchor up by one.
			name: "deletion above → moved",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,5 +1,4 @@\n l1\n-l2\n l3\n l4\n l5\n",
			path: "f.go",
			line: 8,
			want: LineMapResult{Status: LineMapMoved, Line: 7},
		},
		{
			// A context line inside a hunk, untouched, but shifted by an
			// addition earlier in the same hunk.
			name: "context line shifted within hunk → moved",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,4 +1,5 @@\n a\n+X\n b\n c\n d\n",
			path: "f.go",
			line: 4, // "d"
			want: LineMapResult{Status: LineMapMoved, Line: 5},
		},
		{
			// Whole file deleted: every old line is a deletion → outdated.
			name: "deleted file → outdated",
			diff: "diff --git a/f.go b/f.go\n" +
				"deleted file mode 100644\n--- a/f.go\n+++ /dev/null\n" +
				"@@ -1,3 +0,0 @@\n-l1\n-l2\n-l3\n",
			path: "f.go",
			line: 2,
			want: LineMapResult{Status: LineMapOutdated},
		},
		{
			// The file does not appear in the diff → unchanged between A and B.
			name: "file not in diff → unchanged",
			diff: "diff --git a/other.go b/other.go\n" +
				"@@ -1,1 +1,2 @@\n a\n+b\n",
			path: "f.go",
			line: 5,
			want: LineMapResult{Status: LineMapUnchanged, Line: 5},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseLineMap(c.diff).MapComment(c.path, c.line, nil)
			if got != c.want {
				t.Errorf("MapComment(%q, %d) = %+v, want %+v", c.path, c.line, got, c.want)
			}
		})
	}
}

// --- MapComment: multi-line ranges ---

func TestMapComment_MultiLine(t *testing.T) {
	cases := []struct {
		name      string
		diff      string
		path      string
		startLine int
		line      int
		want      LineMapResult
	}{
		{
			// An edit lands inside the [2,5] span → the range no longer
			// anchors the same block.
			name: "range straddling a change → outdated",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,6 +1,6 @@\n a\n b\n-c\n+C\n d\n e\n f\n",
			path:      "f.go",
			startLine: 2,
			line:      5,
			want:      LineMapResult{Status: LineMapOutdated},
		},
		{
			// An insertion strictly inside the span breaks block contiguity.
			name: "insertion inside range → outdated",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,6 +1,7 @@\n a\n b\n c\n+X\n d\n e\n f\n",
			path:      "f.go",
			startLine: 2, // b
			line:      5, // e
			want:      LineMapResult{Status: LineMapOutdated},
		},
		{
			// Clean block shifted down by an insertion above → moved as a unit.
			name: "clean range shifted → moved",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,3 +1,4 @@\n+X\n l1\n l2\n l3\n",
			path:      "f.go",
			startLine: 8,
			line:      9,
			want:      LineMapResult{Status: LineMapMoved, StartLine: 9, Line: 10},
		},
		{
			// Change far below the span; the range is untouched and unshifted.
			name: "clean range unshifted → unchanged",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -8,3 +8,3 @@\n l8\n l9\n-l10\n+L10\n",
			path:      "f.go",
			startLine: 2,
			line:      3,
			want:      LineMapResult{Status: LineMapUnchanged, StartLine: 2, Line: 3},
		},
		{
			// The end of the span is the edited line → outdated.
			name: "endpoint edited → outdated",
			diff: "diff --git a/f.go b/f.go\n" +
				"@@ -1,4 +1,4 @@\n a\n b\n c\n-d\n+D\n",
			path:      "f.go",
			startLine: 1,
			line:      4, // d, edited
			want:      LineMapResult{Status: LineMapOutdated},
		},
		{
			// File not in the diff → the whole range maps unchanged.
			name:      "range, file not in diff → unchanged",
			diff:      "diff --git a/other.go b/other.go\n@@ -1,1 +1,2 @@\n a\n+b\n",
			path:      "f.go",
			startLine: 10,
			line:      12,
			want:      LineMapResult{Status: LineMapUnchanged, StartLine: 10, Line: 12},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sl := c.startLine
			got := ParseLineMap(c.diff).MapComment(c.path, c.line, &sl)
			if got != c.want {
				t.Errorf("MapComment(%q, start=%d, line=%d) = %+v, want %+v",
					c.path, c.startLine, c.line, got, c.want)
			}
		})
	}
}

// --- multi-hunk: deltas from earlier hunks accumulate for later anchors ---

func TestMapComment_MultiHunkOffsetAccumulates(t *testing.T) {
	// Hunk 1 nets +1 (an insertion), hunk 2 nets -1 (a deletion). An anchor
	// after both should be shifted by their sum (0).
	diff := "diff --git a/f.go b/f.go\n" +
		"@@ -1,3 +1,4 @@\n l1\n+X\n l2\n l3\n" +
		"@@ -20,4 +21,3 @@\n l20\n-l21\n l22\n l23\n"
	m := ParseLineMap(diff)

	// Between the two hunks: only hunk 1's +1 applies.
	if got := m.MapComment("f.go", 10, nil); got != (LineMapResult{Status: LineMapMoved, Line: 11}) {
		t.Errorf("line 10 = %+v, want moved→11", got)
	}
	// After both hunks: +1 then -1 nets to 0 → unchanged.
	if got := m.MapComment("f.go", 30, nil); got != (LineMapResult{Status: LineMapUnchanged, Line: 30}) {
		t.Errorf("line 30 = %+v, want unchanged→30", got)
	}
}

// --- patch-set parser parity (GetCompareFiles fallback) ---

func TestMapComment_FromPatches(t *testing.T) {
	files := []PRFile{
		{Filename: "f.go", Patch: "@@ -1,4 +1,5 @@\n+X\n a\n b\n c\n d"},
		{Filename: "g.go", Patch: "@@ -1,4 +1,4 @@\n a\n b\n-c\n+c2\n d"},
	}
	m := ParseLineMapFromPatches(files)

	if got := m.MapComment("f.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 4}) {
		t.Errorf("f.go line 3 = %+v, want moved→4", got)
	}
	if got := m.MapComment("g.go", 3, nil); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("g.go line 3 = %+v, want outdated", got)
	}
	// A file not in the changed-files list maps unchanged.
	if got := m.MapComment("h.go", 7, nil); got != (LineMapResult{Status: LineMapUnchanged, Line: 7}) {
		t.Errorf("h.go line 7 = %+v, want unchanged→7", got)
	}
}

// A file the files API listed but whose patch was omitted (binary, or too large
// for the diff) carries no line-level mapping data — the anchor can't be proven
// fresh, so it maps outdated rather than falsely unchanged.
func TestMapComment_PresentButNoHunks(t *testing.T) {
	// From the patch fallback: the file is in the changed list with an empty patch.
	pm := ParseLineMapFromPatches([]PRFile{{Filename: "big.go", Patch: ""}})
	if got := pm.MapComment("big.go", 10, nil); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("omitted-patch file line 10 = %+v, want outdated", got)
	}
	sl := 8
	if got := pm.MapComment("big.go", 12, &sl); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("omitted-patch file range = %+v, want outdated", got)
	}

	// From unified diff text: a binary file change ("diff --git" header, no @@).
	binary := "diff --git a/img.png b/img.png\n" +
		"index 0000000..1111111 100644\n" +
		"Binary files a/img.png and b/img.png differ\n"
	if got := ParseLineMap(binary).MapComment("img.png", 1, nil); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("binary file = %+v, want outdated", got)
	}

	// A mode-only change (chmod): "diff --git" header + old/new mode, no @@.
	// Content is identical, but with no line-level data we stay conservative.
	modeOnly := "diff --git a/run.sh b/run.sh\n" +
		"old mode 100644\nnew mode 100755\n"
	if got := ParseLineMap(modeOnly).MapComment("run.sh", 4, nil); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("mode-only change = %+v, want outdated", got)
	}
}

// A pure-insertion hunk (oldLen 0, e.g. "-N,0" from --unified=0) inserts after
// old line N: line N itself must not absorb the hunk's delta, while N+1 onward
// shifts by the inserted count.
func TestMapComment_PureInsertionHunk(t *testing.T) {
	// Insert two lines after old line 2 (between old lines 2 and 3).
	diff := "diff --git a/f.go b/f.go\n@@ -2,0 +3,2 @@\n+X\n+Y\n"
	m := ParseLineMap(diff)

	if got := m.MapComment("f.go", 1, nil); got != (LineMapResult{Status: LineMapUnchanged, Line: 1}) {
		t.Errorf("line 1 = %+v, want unchanged→1", got)
	}
	// Old line 2 is *before* the insertion → not shifted.
	if got := m.MapComment("f.go", 2, nil); got != (LineMapResult{Status: LineMapUnchanged, Line: 2}) {
		t.Errorf("line 2 = %+v, want unchanged→2 (insertion is after it)", got)
	}
	// Old line 3 onward is after the insertion → shifted by +2.
	if got := m.MapComment("f.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 5}) {
		t.Errorf("line 3 = %+v, want moved→5", got)
	}
}

// --- renamed files: anchor carries the old path; result reports the new one ---

func TestMapComment_Rename(t *testing.T) {
	// Rename-only (100% similarity, no hunks): content byte-identical, line
	// numbers preserved, but relocated → moved + new path.
	renameOnly := "diff --git a/old.go b/new.go\n" +
		"similarity index 100%\nrename from old.go\nrename to new.go\n"
	if got := ParseLineMap(renameOnly).MapComment("old.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 3, Path: "new.go"}) {
		t.Errorf("rename-only single = %+v, want moved→new.go:3", got)
	}
	sl := 2
	if got := ParseLineMap(renameOnly).MapComment("old.go", 4, &sl); got != (LineMapResult{Status: LineMapMoved, Line: 4, StartLine: 2, Path: "new.go"}) {
		t.Errorf("rename-only range = %+v, want moved→new.go:2-4", got)
	}

	// Rename + edits: an insertion above shifts the anchor; still a move, now
	// also to the new path.
	renameEdit := "diff --git a/old.go b/new.go\n" +
		"similarity index 80%\nrename from old.go\nrename to new.go\n" +
		"--- a/old.go\n+++ b/new.go\n" +
		"@@ -1,4 +1,5 @@\n+X\n a\n b\n c\n d\n"
	if got := ParseLineMap(renameEdit).MapComment("old.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 4, Path: "new.go"}) {
		t.Errorf("rename+edit shift = %+v, want moved→new.go:4", got)
	}

	// Rename + edit *on the anchored line* → outdated (no Path; nothing usable).
	renameEditLine := "diff --git a/old.go b/new.go\n" +
		"similarity index 80%\nrename from old.go\nrename to new.go\n" +
		"@@ -1,4 +1,4 @@\n a\n b\n-c\n+c2\n d\n"
	if got := ParseLineMap(renameEditLine).MapComment("old.go", 3, nil); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("rename+edit on line = %+v, want outdated", got)
	}

	// A copy (copy from/copy to) leaves the source in place — it must NOT be
	// treated as a rename, so the anchor on the original maps unchanged.
	copyDiff := "diff --git a/orig.go b/dup.go\n" +
		"similarity index 100%\ncopy from orig.go\ncopy to dup.go\n"
	if got := ParseLineMap(copyDiff).MapComment("orig.go", 2, nil); got != (LineMapResult{Status: LineMapUnchanged, Line: 2}) {
		t.Errorf("copy source = %+v, want unchanged→2 (copies don't redirect)", got)
	}
}

func TestMapComment_RenameFromPatches(t *testing.T) {
	// Pure rename via the files API: status "renamed", zero add/del, empty patch.
	pure := ParseLineMapFromPatches([]PRFile{
		{Filename: "new.go", Status: "renamed", PreviousFilename: "old.go"},
	})
	if got := pure.MapComment("old.go", 5, nil); got != (LineMapResult{Status: LineMapMoved, Line: 5, Path: "new.go"}) {
		t.Errorf("patch pure rename = %+v, want moved→new.go:5", got)
	}

	// Rename + edits with a patch present → map through it, report the new path.
	edited := ParseLineMapFromPatches([]PRFile{{
		Filename: "new.go", Status: "renamed", PreviousFilename: "old.go",
		Additions: 1, Patch: "@@ -1,4 +1,5 @@\n+X\n a\n b\n c\n d",
	}})
	if got := edited.MapComment("old.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 4, Path: "new.go"}) {
		t.Errorf("patch rename+edit = %+v, want moved→new.go:4", got)
	}

	// Rename + edits whose patch was omitted (oversized diff): nonzero counts but
	// no hunks → not content-identical → outdated.
	omitted := ParseLineMapFromPatches([]PRFile{{
		Filename: "new.go", Status: "renamed", PreviousFilename: "old.go",
		Additions: 3, Deletions: 1, Patch: "",
	}})
	if got := omitted.MapComment("old.go", 5, nil); got != (LineMapResult{Status: LineMapOutdated}) {
		t.Errorf("patch rename omitted = %+v, want outdated", got)
	}
}

// --- CRLF line endings must not leak \r into path keys or rename paths ---

func TestMapComment_CRLF(t *testing.T) {
	// A normal changed file must key cleanly so a clean anchor resolves and maps.
	changed := "diff --git a/f.go b/f.go\r\n@@ -1,4 +1,5 @@\r\n+X\r\n a\r\n b\r\n c\r\n d\r\n"
	if got := ParseLineMap(changed).MapComment("f.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 4}) {
		t.Errorf("CRLF changed file = %+v, want moved→4", got)
	}

	// A rename must index under the clean old path, not "old.go\r".
	rename := "diff --git a/old.go b/new.go\r\n" +
		"similarity index 100%\r\nrename from old.go\r\nrename to new.go\r\n"
	if got := ParseLineMap(rename).MapComment("old.go", 3, nil); got != (LineMapResult{Status: LineMapMoved, Line: 3, Path: "new.go"}) {
		t.Errorf("CRLF rename = %+v, want moved→new.go:3", got)
	}
}

// --- empty / degenerate inputs ---

func TestMapComment_EmptyDiff(t *testing.T) {
	m := ParseLineMap("")
	if got := m.MapComment("f.go", 5, nil); got != (LineMapResult{Status: LineMapUnchanged, Line: 5}) {
		t.Errorf("empty diff = %+v, want unchanged→5", got)
	}
}

func TestMapComment_MalformedStartGreaterThanLine(t *testing.T) {
	diff := "diff --git a/f.go b/f.go\n@@ -1,3 +1,3 @@\n a\n b\n c\n"
	sl := 5
	if got := ParseLineMap(diff).MapComment("f.go", 2, &sl); got.Status != LineMapOutdated {
		t.Errorf("start>line = %+v, want outdated", got)
	}
}
