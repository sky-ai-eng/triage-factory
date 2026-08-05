package main

import (
	"fmt"
	"strconv"
	"strings"
)

type changedLine struct {
	No   int // line number in the file this side of the diff
	Text string
}

type fileDiff struct {
	OldPath string // empty when the file was added
	NewPath string // empty when the file was deleted
	Binary  bool
	Added   []changedLine
	Removed []changedLine
}

// Path returns the name to classify the file under: the post-image if there is
// one, so a rename is bucketed by where the code ended up.
func (f fileDiff) Path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// parseUnifiedDiff reads `git diff --unified=0` output.
//
// The one trap is that a removed line whose content begins with "--" renders as
// "---…", which is byte-identical to a file header. Headers only ever appear
// before a file's first hunk, so tracking that boundary is what keeps a diff
// that touches SQL comments or markdown rules from being misread as a new file.
func parseUnifiedDiff(out string) ([]fileDiff, error) {
	var files []fileDiff
	var cur *fileDiff
	var inHunk bool
	oldLine, newLine := 0, 0

	flush := func() {
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &fileDiff{}
			inHunk = false

		case cur == nil:
			// Preamble or trailing blank; nothing to attribute it to.

		case !inHunk && strings.HasPrefix(line, "--- "):
			cur.OldPath = headerPath(line[4:])

		case !inHunk && strings.HasPrefix(line, "+++ "):
			cur.NewPath = headerPath(line[4:])

		case !inHunk && (strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch")):
			cur.Binary = true

		case strings.HasPrefix(line, "@@"):
			o, n, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			oldLine, newLine = o, n
			inHunk = true

		case !inHunk:
			// Mode changes, similarity index, rename from/to.

		case strings.HasPrefix(line, "+"):
			cur.Added = append(cur.Added, changedLine{No: newLine, Text: line[1:]})
			newLine++

		case strings.HasPrefix(line, "-"):
			cur.Removed = append(cur.Removed, changedLine{No: oldLine, Text: line[1:]})
			oldLine++

		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" — not a line of either file.

		default:
			// Context, which --unified=0 should never emit, but a mis-set
			// diff.context in user config would; keep the counters honest.
			oldLine++
			newLine++
		}
	}
	flush()
	return files, nil
}

// headerPath strips the a//b/ prefix git puts on diff header paths, unquoting
// first when the name needed escaping.
func headerPath(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if strings.HasPrefix(s, `"`) {
		if unq, err := strconv.Unquote(s); err == nil {
			s = unq
		}
	}
	if s == "/dev/null" {
		return ""
	}
	if len(s) > 2 && (s[:2] == "a/" || s[:2] == "b/") {
		return s[2:]
	}
	return s
}

// parseHunkHeader reads "@@ -old[,n] +new[,n] @@ …" and returns the first line
// number on each side.
func parseHunkHeader(line string) (int, int, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, fmt.Errorf("malformed hunk header: %q", line)
	}
	old, err := hunkStart(fields[1][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	newStart, err := hunkStart(fields[2][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	return old, newStart, nil
}

func hunkStart(s string) (int, error) {
	if i := strings.Index(s, ","); i >= 0 {
		s = s[:i]
	}
	return strconv.Atoi(s)
}
