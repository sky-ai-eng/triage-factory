package github

import (
	"strconv"
	"strings"
)

// DiffLines parses a unified diff and returns a map of file path → set of commentable line numbers.
// These are the "new" side line numbers (right side) that GitHub will accept for review comments.
func DiffLines(diff string) map[string]map[int]bool {
	result := make(map[string]map[int]bool)
	var currentFile string
	var lineNum int

	for _, raw := range strings.Split(diff, "\n") {
		// Strip the trailing CR of a CRLF diff so it never leaks into a file-path
		// key (a "path\r" key would never match a clean lookup).
		line := strings.TrimSuffix(raw, "\r")

		if strings.HasPrefix(line, "diff --git") {
			// Extract file path from "diff --git a/path b/path"
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				currentFile = parts[1]
				result[currentFile] = make(map[int]bool)
			}
			continue
		}

		if strings.HasPrefix(line, "@@") && currentFile != "" {
			// Parse hunk header: @@ -old,count +new,count @@
			lineNum = parseHunkNewStart(line)
			continue
		}

		if currentFile == "" || lineNum == 0 {
			continue
		}

		switch {
		case strings.HasPrefix(line, "-"):
			// Deletion — not on the new side; don't advance line counter.
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, " "):
			// Added or context line — commentable on the new side.
			result[currentFile][lineNum] = true
			lineNum++
		default:
			// Skip "\ No newline at end of file" markers and trailing empty strings.
		}
	}

	return result
}

// DiffLinesFromPatches builds the same file → commentable-line-set map as
// DiffLines, but from the per-file patch strings returned by GetPRFiles.
// Used as a fallback when the full PR diff is too large (HTTP 406).
func DiffLinesFromPatches(files []PRFile) map[string]map[int]bool {
	result := make(map[string]map[int]bool)
	for _, f := range files {
		result[f.Filename] = parsePatchLines(f.Patch)
	}
	return result
}

// parsePatchLines extracts commentable new-side line numbers from a single
// file's patch string (as returned by GitHub's PR files API).
func parsePatchLines(patch string) map[int]bool {
	result := make(map[int]bool)
	var lineNum int
	for _, raw := range strings.Split(patch, "\n") {
		line := strings.TrimSuffix(raw, "\r") // CRLF-safe, mirroring DiffLines
		if strings.HasPrefix(line, "@@") {
			lineNum = parseHunkNewStart(line)
			continue
		}
		if lineNum == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "-"):
			// Deletion — not on new side; don't advance.
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, " "):
			result[lineNum] = true
			lineNum++
		default:
			// Skip "\ No newline at end of file" markers and trailing empty strings.
		}
	}
	return result
}

// Hunk is a single contiguous range of new-side line numbers in a diff.
// Within one hunk, every commentable new-side line falls in [NewStart, NewEnd]
// (deletions don't advance the new-side counter; additions and context lines
// each advance by one), so a range is sufficient — no need to store a set.
//
// GitHub requires multi-line review comments to have start_line and line in
// the same hunk; storing per-hunk ranges is what lets us validate that.
type Hunk struct {
	NewStart int // first new-side line in the hunk (inclusive)
	NewEnd   int // last new-side line in the hunk (inclusive)
}

// DiffHunks parses a unified diff and returns a map of file path → ordered
// list of hunks (new-side ranges). Files appear in the map even if they
// have no commentable lines on the new side (e.g., pure-deletion files),
// in which case the slice is empty.
func DiffHunks(diff string) map[string][]Hunk {
	result := make(map[string][]Hunk)
	var currentFile string
	var inHunk bool
	var hunkStart, lineNum int

	flush := func() {
		if !inHunk || currentFile == "" {
			return
		}
		// lineNum advanced past the last commentable line, so end is lineNum-1.
		if lineNum > hunkStart {
			result[currentFile] = append(result[currentFile], Hunk{NewStart: hunkStart, NewEnd: lineNum - 1})
		}
		inHunk = false
	}

	for _, raw := range strings.Split(diff, "\n") {
		// Strip the trailing CR of a CRLF diff so it never leaks into a file-path
		// key (a "path\r" key would never match a clean lookup).
		line := strings.TrimSuffix(raw, "\r")

		if strings.HasPrefix(line, "diff --git") {
			flush()
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				currentFile = parts[1]
				if _, ok := result[currentFile]; !ok {
					result[currentFile] = nil
				}
			} else {
				currentFile = ""
			}
			continue
		}

		if strings.HasPrefix(line, "@@") && currentFile != "" {
			flush()
			start := parseHunkNewStart(line)
			if start == 0 {
				continue
			}
			hunkStart = start
			lineNum = start
			inHunk = true
			continue
		}

		if !inHunk {
			continue
		}

		switch {
		case strings.HasPrefix(line, "-"):
			// Deletion — not on the new side; don't advance.
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, " "):
			lineNum++
		default:
			// "\ No newline at end of file" markers and the trailing empty
			// string from strings.Split don't advance the new-side counter.
		}
	}
	flush()

	return result
}

// DiffHunksFromPatches mirrors DiffLinesFromPatches: builds the same
// file → hunks map but from per-file patch strings (used as a fallback
// when the full PR diff is too large for the API to return).
func DiffHunksFromPatches(files []PRFile) map[string][]Hunk {
	result := make(map[string][]Hunk)
	for _, f := range files {
		result[f.Filename] = parsePatchHunks(f.Patch)
	}
	return result
}

func parsePatchHunks(patch string) []Hunk {
	var hunks []Hunk
	var inHunk bool
	var hunkStart, lineNum int

	flush := func() {
		if !inHunk {
			return
		}
		if lineNum > hunkStart {
			hunks = append(hunks, Hunk{NewStart: hunkStart, NewEnd: lineNum - 1})
		}
		inHunk = false
	}

	for _, raw := range strings.Split(patch, "\n") {
		line := strings.TrimSuffix(raw, "\r") // CRLF-safe, mirroring DiffHunks
		if strings.HasPrefix(line, "@@") {
			flush()
			start := parseHunkNewStart(line)
			if start == 0 {
				continue
			}
			hunkStart = start
			lineNum = start
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(line, "-"):
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, " "):
			lineNum++
		}
	}
	flush()

	return hunks
}

// ValidateCommentRange checks that a review comment's line range is
// acceptable to GitHub:
//   - the file must be in the diff,
//   - line must fall inside some hunk,
//   - if startLine is set: startLine ≤ line, and both endpoints must
//     fall inside the *same* hunk (GitHub rejects cross-hunk ranges
//     with HTTP 422 at review submission time).
//
// Returns "" on success, or an agent-friendly error message that
// includes the file's hunk list so the caller can pick a valid range
// on retry.
func ValidateCommentRange(hunks map[string][]Hunk, path string, line int, startLine *int) string {
	fileHunks, ok := hunks[path]
	if !ok {
		return "file '" + path + "' is not in the diff"
	}

	lineHunk := findHunk(fileHunks, line)
	if lineHunk < 0 {
		return "line " + strconv.Itoa(line) + " in '" + path +
			"' is not part of the diff (hunks for '" + path + "': " + formatHunks(fileHunks) + ")"
	}

	if startLine == nil {
		return ""
	}

	if *startLine > line {
		return "start_line " + strconv.Itoa(*startLine) + " must be ≤ line " + strconv.Itoa(line)
	}

	startHunk := findHunk(fileHunks, *startLine)
	if startHunk < 0 {
		return "start_line " + strconv.Itoa(*startLine) + " in '" + path +
			"' is not part of the diff (hunks for '" + path + "': " + formatHunks(fileHunks) + ")"
	}

	if startHunk != lineHunk {
		return "start_line " + strconv.Itoa(*startLine) + " and line " + strconv.Itoa(line) +
			" must be in the same diff hunk (hunks for '" + path + "': " + formatHunks(fileHunks) + ")"
	}

	return ""
}

func findHunk(hunks []Hunk, line int) int {
	for i, h := range hunks {
		if line >= h.NewStart && line <= h.NewEnd {
			return i
		}
	}
	return -1
}

func formatHunks(hunks []Hunk) string {
	if len(hunks) == 0 {
		return "none"
	}
	parts := make([]string, len(hunks))
	for i, h := range hunks {
		parts[i] = "[" + strconv.Itoa(h.NewStart) + "–" + strconv.Itoa(h.NewEnd) + "]"
	}
	return strings.Join(parts, ", ")
}

func parseHunkNewStart(hunkHeader string) int {
	// @@ -old,count +new,count @@ optional section
	plusIdx := strings.Index(hunkHeader, "+")
	if plusIdx < 0 {
		return 0
	}
	rest := hunkHeader[plusIdx+1:]
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
