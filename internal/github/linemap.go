package github

import (
	"strconv"
	"strings"
)

// Forward diff line-mapping primitive (TFAC-497).
//
// A staged review comment is anchored to the commit it was authored against
// (ReviewArtifactComment.CommitSHA) on a new-side line number in that commit's
// frame. When the reviewed PR advances to a newer head, we must answer, per
// comment: does its (path, line[, start_line]) still point at the same code?
//
// GitHub computes this internally for native pending reviews ("outdated" vs.
// auto-tracked), but TFAC-494 removed the live GitHub pending review, so we
// compute it ourselves from the anchor→target diff.
//
// Model: the diff is read as A→B, where commit A (the comment's anchor) is the
// OLD side and commit B (the PR's current head) is the NEW side. A comment's
// line is therefore an OLD-side line number in this diff; mapping forward means
// translating it to its NEW-side line number in B's frame:
//
//   - a context line (' ') survives — its new-side number may shift if lines
//     were inserted/deleted above it within the hunk or in earlier hunks;
//   - a deletion ('-') on the anchored line itself means the line was edited
//     (a modification is a '-' old line plus a '+' new line) or removed — the
//     anchor is outdated;
//   - lines outside every hunk are unchanged content, shifted by the cumulative
//     net delta (additions minus deletions) of all preceding hunks.
//
// DiffHunks (diffparse.go) keeps only new-side ranges, which is enough for
// validating a fresh comment but insufficient to forward-map an old-side
// anchor — we need the old↔new correspondence and the deletion markers — so
// this builds its own per-file parse retaining both sides.

// LineMapStatus is the typed verdict for a forward-mapped comment anchor.
type LineMapStatus string

const (
	// LineMapUnchanged: the anchored code is identical and its line number did
	// not move (new-side line == old-side line).
	LineMapUnchanged LineMapStatus = "unchanged"
	// LineMapMoved: the anchored code is identical but its line number shifted
	// (insertions/deletions above moved it; the content the comment points at
	// is the same).
	LineMapMoved LineMapStatus = "moved"
	// LineMapOutdated: the anchored line's content changed or was deleted, the
	// file/hunk no longer contains it, or (for a multi-line range) a change
	// falls inside the span — the comment no longer points at the same code.
	LineMapOutdated LineMapStatus = "outdated"
)

// LineMapResult is the forward-mapping verdict plus the remapped new-side
// position. Line and StartLine are the comment's new-side line and start_line
// in the target commit; both are 0 when Status is LineMapOutdated, and
// StartLine is 0 for a single-line comment (one with no start_line).
type LineMapResult struct {
	Status    LineMapStatus
	Line      int
	StartLine int
}

// LineMap is a parsed A→B diff prepared for forward-mapping comment anchors.
// Parse once (the source diff is shared across a draft's comments) and call
// MapComment per comment.
type LineMap struct {
	files map[string][]lineMapHunk
}

// lineMapHunk is one diff hunk retaining both old-side and new-side starts plus
// the ordered op kinds, so a single old-side line can be walked forward to its
// new-side line (or found deleted). oldLen/newLen are the old-side and new-side
// span lengths (precomputed for the between-hunks offset accounting).
type lineMapHunk struct {
	oldStart int
	newStart int
	oldLen   int    // count of context (' ') + deletion ('-') lines
	newLen   int    // count of context (' ') + addition ('+') lines
	ops      []byte // ordered op kinds: ' ', '-', '+'
}

// ParseLineMap parses unified diff text (A→B, multi-file with "diff --git"
// headers) into a LineMap. New-side line numbers are keyed by the diff's b/
// path, matching DiffHunks.
func ParseLineMap(diff string) LineMap {
	m := LineMap{files: make(map[string][]lineMapHunk)}
	var currentFile string
	var cur *lineMapHunk

	flush := func() {
		if cur != nil && currentFile != "" {
			m.files[currentFile] = append(m.files[currentFile], *cur)
		}
		cur = nil
	}

	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git") {
			flush()
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				currentFile = parts[1]
				if _, ok := m.files[currentFile]; !ok {
					m.files[currentFile] = nil
				}
			} else {
				currentFile = ""
			}
			continue
		}

		if strings.HasPrefix(line, "@@") {
			flush()
			if currentFile == "" {
				continue
			}
			// Keep the hunk even when one side starts at 0 (a deleted file's
			// "+0,0" or a new file's "-0,0") — unlike DiffHunks, which only
			// cares about commentable new-side ranges, the forward map needs
			// the deletion side to report those lines outdated.
			cur = &lineMapHunk{
				oldStart: parseHunkOldStart(line),
				newStart: parseHunkNewStart(line),
			}
			continue
		}

		if cur == nil {
			// Inside a file header (index/---/+++) or between files — also where
			// "--- a/x" / "+++ b/x" are safely skipped, since they precede the
			// first "@@" of every file.
			continue
		}

		appendOp(cur, line)
	}
	flush()

	return m
}

// ParseLineMapFromPatches builds the same LineMap from the per-file patch
// strings returned by GetCompareFiles / GetPRFiles — the JSON fallback when the
// raw diff media type is refused (HTTP 406). Mirrors DiffHunksFromPatches.
func ParseLineMapFromPatches(files []PRFile) LineMap {
	m := LineMap{files: make(map[string][]lineMapHunk)}
	for _, f := range files {
		m.files[f.Filename] = parsePatchLineMapHunks(f.Patch)
	}
	return m
}

func parsePatchLineMapHunks(patch string) []lineMapHunk {
	var hunks []lineMapHunk
	var cur *lineMapHunk

	flush := func() {
		if cur != nil {
			hunks = append(hunks, *cur)
		}
		cur = nil
	}

	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			flush()
			cur = &lineMapHunk{
				oldStart: parseHunkOldStart(line),
				newStart: parseHunkNewStart(line),
			}
			continue
		}
		if cur == nil {
			continue
		}
		appendOp(cur, line)
	}
	flush()

	return hunks
}

// appendOp records one hunk-body line on the current hunk. Lines that advance
// neither side ("\ No newline at end of file", the trailing empty string from
// strings.Split) are ignored.
func appendOp(h *lineMapHunk, line string) {
	switch {
	case strings.HasPrefix(line, " "):
		h.ops = append(h.ops, ' ')
		h.oldLen++
		h.newLen++
	case strings.HasPrefix(line, "-"):
		h.ops = append(h.ops, '-')
		h.oldLen++
	case strings.HasPrefix(line, "+"):
		h.ops = append(h.ops, '+')
		h.newLen++
	default:
		// "\ No newline at end of file" markers and empty strings.
	}
}

// MapComment forward-maps one comment anchor (path, line, startLine?) from the
// diff's old side (commit A) to its new side (commit B).
//
//   - Single-line (startLine == nil): unchanged if the line survives at the same
//     number, moved if it survives at a different number, outdated if its content
//     changed or it was deleted.
//   - Multi-line (startLine != nil): the whole span [startLine, line] must
//     survive as a contiguous, content-identical block. Any line deleted/edited
//     inside the span, or an insertion that splits it, makes the comment
//     outdated (a multi-line range straddling a change no longer anchors the
//     same code). Otherwise unchanged (no shift) or moved (shifted as a block).
//
// A file absent from the diff is unchanged between A and B, so its lines map to
// themselves (unchanged). A file present in the diff but carrying no parsed
// hunks is the opposite — it changed in a way we have no line-level mapping for
// (a binary "files differ", a rename-only / mode-only change, or a patch the
// files API omitted for an oversized diff) — so we report outdated rather than
// falsely claim the anchor is fresh. (A file renamed *with* content changes is
// the known gap — the diff keys it under the new path; out of scope here.)
func (m LineMap) MapComment(path string, line int, startLine *int) LineMapResult {
	hunks, ok := m.files[path]
	if !ok {
		// File absent from the diff: unchanged between A and B, anchor unshifted.
		if startLine != nil {
			return LineMapResult{Status: LineMapUnchanged, Line: line, StartLine: *startLine}
		}
		return LineMapResult{Status: LineMapUnchanged, Line: line}
	}
	if len(hunks) == 0 {
		// File is in the diff but has no line-level mapping data (binary,
		// rename-only / mode-only, or an omitted oversized-diff patch). We can't
		// prove the anchor still points at the same code, so be conservative: a
		// false "outdated" only over-flags for re-anchoring, whereas a false
		// "unchanged" would land the comment on code that may have moved.
		return LineMapResult{Status: LineMapOutdated}
	}

	if startLine == nil {
		newLine, outdated := mapOldLine(hunks, line)
		if outdated {
			return LineMapResult{Status: LineMapOutdated}
		}
		if newLine == line {
			return LineMapResult{Status: LineMapUnchanged, Line: newLine}
		}
		return LineMapResult{Status: LineMapMoved, Line: newLine}
	}

	start := *startLine
	if start > line {
		// Malformed anchor (ValidateCommentRange enforces start ≤ line at
		// staging time); treat as no longer mappable.
		return LineMapResult{Status: LineMapOutdated}
	}

	// Map every old line in [start, line]. The block survives only if all lines
	// survive and remap contiguously (each one immediately after the previous):
	// a deleted/edited interior line maps outdated, and an interior insertion
	// breaks contiguity — either way the anchored block changed.
	var newStart, prev int
	for k := start; k <= line; k++ {
		nl, outdated := mapOldLine(hunks, k)
		if outdated {
			return LineMapResult{Status: LineMapOutdated}
		}
		if k == start {
			newStart = nl
		} else if nl != prev+1 {
			return LineMapResult{Status: LineMapOutdated}
		}
		prev = nl
	}
	newEnd := prev

	if newStart == start && newEnd == line {
		return LineMapResult{Status: LineMapUnchanged, Line: newEnd, StartLine: newStart}
	}
	return LineMapResult{Status: LineMapMoved, Line: newEnd, StartLine: newStart}
}

// mapOldLine translates a single old-side line number to its new-side line
// number. outdated is true when the line was deleted or edited (a '-' on that
// exact old line). Hunks are in ascending old-side order (as diffs always are).
func mapOldLine(hunks []lineMapHunk, oldLine int) (newLine int, outdated bool) {
	offset := 0
	for _, h := range hunks {
		if oldLine < h.oldStart {
			// Before this hunk, in an unchanged region governed by the net
			// delta of all preceding hunks.
			return oldLine + offset, false
		}
		oldEnd := h.oldStart + h.oldLen - 1
		if oldLine <= oldEnd {
			return walkHunk(h, oldLine)
		}
		offset += h.newLen - h.oldLen
	}
	// After the last hunk: unchanged content, shifted by the cumulative delta.
	return oldLine + offset, false
}

// walkHunk finds oldLine within a single hunk, returning its new-side line
// number (context line) or outdated (deletion on that line).
func walkHunk(h lineMapHunk, oldLine int) (newLine int, outdated bool) {
	oldCur := h.oldStart
	newCur := h.newStart
	for _, op := range h.ops {
		switch op {
		case ' ':
			if oldCur == oldLine {
				return newCur, false
			}
			oldCur++
			newCur++
		case '-':
			if oldCur == oldLine {
				return 0, true
			}
			oldCur++
		case '+':
			newCur++
		}
	}
	// oldLine was within the hunk's old span but matched no op — a malformed
	// hunk; report outdated rather than guessing a new position.
	return 0, true
}

// parseHunkOldStart parses the old-side (left) start line from a hunk header,
// mirroring parseHunkNewStart for the "-old,count" coordinate.
func parseHunkOldStart(hunkHeader string) int {
	// @@ -old,count +new,count @@ optional section
	minusIdx := strings.Index(hunkHeader, "-")
	if minusIdx < 0 {
		return 0
	}
	rest := hunkHeader[minusIdx+1:]
	commaIdx := strings.Index(rest, ",")
	spaceIdx := strings.Index(rest, " ")

	end := len(rest)
	if commaIdx > 0 && commaIdx < end {
		end = commaIdx
	}
	if spaceIdx > 0 && spaceIdx < end {
		end = spaceIdx
	}

	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
