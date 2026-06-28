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
//
// Renames are handled too: the diff keys a renamed file under its new path, but
// a staged comment carries the *old* path. ParseLineMap records old→new from
// the "rename from"/"rename to" header (copies are intentionally excluded — the
// source still exists); the patch fallback uses PRFile.Status=="renamed" +
// PreviousFilename. A surviving anchor on a renamed file maps to LineMapMoved
// with LineMapResult.Path set to the new path (the anchor relocated even if its
// line number didn't change).

// LineMapStatus is the typed verdict for a forward-mapped comment anchor.
type LineMapStatus string

const (
	// LineMapUnchanged: the anchored code is identical and its line number did
	// not move (new-side line == old-side line).
	LineMapUnchanged LineMapStatus = "unchanged"
	// LineMapMoved: the anchored code is identical but its location changed —
	// its line number shifted (insertions/deletions above moved it) and/or its
	// file was renamed (LineMapResult.Path then holds the new path). The content
	// the comment points at is the same; it just isn't where the anchor said.
	LineMapMoved LineMapStatus = "moved"
	// LineMapOutdated: the anchored line's content changed or was deleted, the
	// file/hunk no longer contains it, or (for a multi-line range) a change
	// falls inside the span — the comment no longer points at the same code.
	LineMapOutdated LineMapStatus = "outdated"
)

// LineMapResult is the forward-mapping verdict plus the remapped new-side
// position. Line and StartLine are the comment's new-side line and start_line in
// the target commit. Both are 0 when Status is LineMapOutdated. StartLine is
// additionally 0 for any single-line comment (the startLine arg was nil),
// regardless of status.
//
// Path, when non-empty, is the new-side path the comment must be re-anchored to
// — set only when the anchored file was renamed between A and B (Status is then
// LineMapMoved). Empty means the path is unchanged (use the comment's original
// path). It is always empty for LineMapOutdated.
type LineMapResult struct {
	Status    LineMapStatus
	Line      int
	StartLine int
	Path      string
}

// LineMap is a parsed A→B diff prepared for forward-mapping comment anchors.
// Parse once (the source diff is shared across a draft's comments) and call
// MapComment per comment.
//
// The files map's *membership* (not slice length) is load-bearing: a path
// absent from the map is unchanged between A and B, while a path present with no
// hunks (nil or empty slice — every file the parser sees is registered, hunks
// appended only as "@@" sections are read) is a file that changed but carries
// no line-level data (binary, rename-only, mode-only, or an omitted oversized
// patch). MapComment maps the former unchanged and the latter outdated, so a
// future writer must not collapse "absent" to "present, empty".
//
// renames maps an old (pre-rename) path to its new path and whether the rename
// preserved content. A staged comment carries the old path, so MapComment
// consults this before concluding a path is absent: the new path's hunks (keyed
// in files) drive the mapping, and the result reports the new path.
type LineMap struct {
	files   map[string][]lineMapHunk
	renames map[string]renameInfo
}

// renameInfo is a rename's new path plus whether it preserved content byte-for-
// byte (a 100%-similarity rename, which carries no hunks). contentIdentical
// lets MapComment tell a pure relocation (line numbers preserved) apart from a
// rename whose edits we have no line data for (an omitted oversized patch),
// which must stay outdated.
type renameInfo struct {
	to               string
	contentIdentical bool
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
	m := LineMap{
		files:   make(map[string][]lineMapHunk),
		renames: make(map[string]renameInfo),
	}
	var currentFile, renameFrom string
	var cur *lineMapHunk
	var sawHunk bool

	flush := func() {
		if cur != nil && currentFile != "" {
			m.files[currentFile] = append(m.files[currentFile], *cur)
		}
		cur = nil
	}
	// finishFile records the just-ended file's rename, if any. A rename with no
	// hunks is a 100%-similarity (content-identical) relocation; with hunks it's
	// a rename + edits.
	finishFile := func() {
		if renameFrom != "" && currentFile != "" {
			m.renames[renameFrom] = renameInfo{to: currentFile, contentIdentical: !sawHunk}
		}
		renameFrom = ""
		sawHunk = false
	}

	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git") {
			flush()
			finishFile()
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
			sawHunk = true
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
			// Inside a file header (index/---/+++/rename/similarity) or between
			// files. "--- a/x" / "+++ b/x" are safely skipped here since they
			// precede the first "@@". "rename from <p>" gives the old path a
			// staged comment is anchored to; "copy from" is deliberately not
			// matched (a copy leaves the source in place).
			if strings.HasPrefix(line, "rename from ") {
				renameFrom = strings.TrimPrefix(line, "rename from ")
			}
			continue
		}

		appendOp(cur, line)
	}
	flush()
	finishFile()

	return m
}

// ParseLineMapFromPatches builds the same LineMap from the per-file patch
// strings returned by GetCompareFiles / GetPRFiles — the JSON fallback when the
// raw diff media type is refused (HTTP 406). Mirrors DiffHunksFromPatches.
func ParseLineMapFromPatches(files []PRFile) LineMap {
	m := LineMap{
		files:   make(map[string][]lineMapHunk),
		renames: make(map[string]renameInfo),
	}
	for _, f := range files {
		hunks := parsePatchLineMapHunks(f.Patch)
		m.files[f.Filename] = hunks
		if f.Status == "renamed" && f.PreviousFilename != "" {
			// A pure rename reports zero additions/deletions and no patch; a
			// rename + edits has hunks (or, if the patch was omitted for an
			// oversized diff, nonzero counts but no hunks — not content-identical,
			// so it stays outdated rather than mapping 1:1).
			m.renames[f.PreviousFilename] = renameInfo{
				to:               f.Filename,
				contentIdentical: len(hunks) == 0 && f.Additions == 0 && f.Deletions == 0,
			}
		}
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
// (a binary "files differ", a mode-only change, or a patch the files API
// omitted for an oversized diff) — so we report outdated rather than falsely
// claim the anchor is fresh. A renamed file is keyed under its new path, so an
// anchor on the old path is redirected through the rename index and, if it
// survives, reported as LineMapMoved with Path set to the new path.
func (m LineMap) MapComment(path string, line int, startLine *int) LineMapResult {
	hunks, ok := m.files[path]
	if !ok {
		// Not changed under this path — but it may have been renamed away from it.
		if r, renamed := m.renames[path]; renamed {
			return m.mapRenamed(r, line, startLine)
		}
		// File absent from the diff: unchanged between A and B, anchor unshifted.
		if startLine != nil {
			return LineMapResult{Status: LineMapUnchanged, Line: line, StartLine: *startLine}
		}
		return LineMapResult{Status: LineMapUnchanged, Line: line}
	}
	if len(hunks) == 0 {
		// File is in the diff but has no line-level mapping data (binary,
		// mode-only, or an omitted oversized-diff patch). We can't prove the
		// anchor still points at the same code, so be conservative: a false
		// "outdated" only over-flags for re-anchoring, whereas a false
		// "unchanged" would land the comment on code that may have moved.
		return LineMapResult{Status: LineMapOutdated}
	}

	newLine, newStart, sameLine, survived := mapSpan(hunks, line, startLine)
	if !survived {
		return LineMapResult{Status: LineMapOutdated}
	}
	if sameLine {
		return LineMapResult{Status: LineMapUnchanged, Line: newLine, StartLine: newStart}
	}
	return LineMapResult{Status: LineMapMoved, Line: newLine, StartLine: newStart}
}

// mapRenamed maps an anchor whose file was renamed (old path → r.to). A rename
// is a relocation, so a surviving anchor is always LineMapMoved (even at the
// same line number) and carries the new path; only a destroyed anchor is
// outdated.
func (m LineMap) mapRenamed(r renameInfo, line int, startLine *int) LineMapResult {
	hunks := m.files[r.to]
	if len(hunks) == 0 {
		if r.contentIdentical {
			// Pure rename: content is byte-identical, line numbers preserved —
			// the anchor just lives at a new path now.
			res := LineMapResult{Status: LineMapMoved, Line: line, Path: r.to}
			if startLine != nil {
				res.StartLine = *startLine
			}
			return res
		}
		// Renamed with edits whose patch we lack (omitted oversized diff): no
		// line data, so we can't prove the anchor survived.
		return LineMapResult{Status: LineMapOutdated}
	}
	newLine, newStart, _, survived := mapSpan(hunks, line, startLine)
	if !survived {
		return LineMapResult{Status: LineMapOutdated}
	}
	return LineMapResult{Status: LineMapMoved, Line: newLine, StartLine: newStart, Path: r.to}
}

// mapSpan maps a single-line (startLine == nil) or multi-line anchor against one
// file's hunks. survived is false when the anchor (or any line in its range) was
// edited/deleted or the range no longer remaps contiguously. sameLine is true
// when a surviving anchor kept its exact line number(s) — the unchanged-vs-moved
// discriminator for a same-path mapping (ignored for a rename, which is always a
// move). newStart is 0 for a single-line anchor.
func mapSpan(hunks []lineMapHunk, line int, startLine *int) (newLine, newStart int, sameLine, survived bool) {
	if startLine == nil {
		nl, outdated := mapOldLine(hunks, line)
		if outdated {
			return 0, 0, false, false
		}
		return nl, 0, nl == line, true
	}

	start := *startLine
	if start > line {
		// Malformed anchor (ValidateCommentRange enforces start ≤ line at
		// staging time); treat as no longer mappable.
		return 0, 0, false, false
	}

	// Map every old line in [start, line]. The block survives only if all lines
	// survive and remap contiguously (each one immediately after the previous):
	// a deleted/edited interior line maps outdated, and an interior insertion
	// breaks contiguity — either way the anchored block changed.
	// prev holds the previous line's mapped position; it's seeded on the first
	// iteration (k == start) before any read, so its zero value is never used.
	var ns, prev int
	for k := start; k <= line; k++ {
		nl, outdated := mapOldLine(hunks, k)
		if outdated {
			return 0, 0, false, false
		}
		if k == start {
			ns = nl
		} else if nl != prev+1 {
			return 0, 0, false, false
		}
		prev = nl
	}
	return prev, ns, ns == start && prev == line, true
}

// mapOldLine translates a single old-side line number to its new-side line
// number. outdated is true when the line was deleted or edited (a '-' on that
// exact old line). Hunks are in ascending old-side order (as diffs always are).
func mapOldLine(hunks []lineMapHunk, oldLine int) (newLine int, outdated bool) {
	offset := 0
	for _, h := range hunks {
		// "Before this hunk" → an unchanged region governed by the net delta of
		// all preceding hunks. A pure-insertion hunk (oldLen == 0, e.g. a
		// "-N,0" hunk from --unified=0) touches no old line and places its
		// content *after* old line N, so old line N itself is still before the
		// insertion and must not absorb this hunk's delta.
		if oldLine < h.oldStart || (h.oldLen == 0 && oldLine == h.oldStart) {
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
