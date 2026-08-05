package main

import "fmt"

type pair struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

func (p *pair) add(side int, n int) {
	if side > 0 {
		p.Added += n
	} else {
		p.Removed += n
	}
}

func (p pair) empty() bool { return p.Added == 0 && p.Removed == 0 }

func (p pair) String() string { return fmt.Sprintf("+%d/-%d", p.Added, p.Removed) }

// buckets are the four rows of the PR template's table.
type buckets struct {
	Code          pair `json:"code"`
	Tests         pair `json:"tests"`
	Documentation pair `json:"documentation"`
	Comments      pair `json:"comments"`
}

type fileResult struct {
	Path     string  `json:"path"`
	Class    string  `json:"class"`
	Excluded bool    `json:"excluded"`
	Buckets  buckets `json:"buckets"`
	Blank    pair    `json:"blank"`
}

type result struct {
	Buckets     buckets      `json:"buckets"`
	Blank       pair         `json:"blank"`
	Generated   pair         `json:"generated"`
	BinaryFiles int          `json:"binaryFiles"`
	Files       int          `json:"fileCount"`
	PerFile     []fileResult `json:"files,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

// route decides which bucket one changed line lands in.
//
// The four buckets are mutually exclusive, so precedence has to be stated
// somewhere and this is it: documentation > comments > tests > code. Comments
// outrank tests deliberately — the row exists to answer "how much of this diff
// is prose rather than behavior", and a comment is prose wherever it lives.
// Blank lines belong to no bucket and are reported separately so the table's
// arithmetic can be reconciled against the diff.
func route(b *buckets, blank *pair, cls fileClass, kind lineKind, side int) {
	if kind == lineBlank {
		blank.add(side, 1)
		return
	}
	switch {
	case cls == classDocs:
		b.Documentation.add(side, 1)
	case kind == lineComment:
		b.Comments.add(side, 1)
	case cls == classTests:
		b.Tests.add(side, 1)
	default:
		b.Code.add(side, 1)
	}
}

func (b *buckets) merge(o buckets) {
	b.Code.Added += o.Code.Added
	b.Code.Removed += o.Code.Removed
	b.Tests.Added += o.Tests.Added
	b.Tests.Removed += o.Tests.Removed
	b.Documentation.Added += o.Documentation.Added
	b.Documentation.Removed += o.Documentation.Removed
	b.Comments.Added += o.Comments.Added
	b.Comments.Removed += o.Comments.Removed
}

// count turns a parsed diff into the table. headRev is empty for a working-tree
// comparison, where post-image content comes from disk instead of a blob.
func count(r *repo, files []fileDiff, baseRev, headRev string, includeGenerated bool) result {
	var res result
	for _, f := range files {
		path := f.Path()
		if path == "" {
			continue
		}
		res.Files++

		fr := fileResult{Path: path}

		if f.Binary {
			res.BinaryFiles++
			fr.Class, fr.Excluded = "binary", true
			res.PerFile = append(res.PerFile, fr)
			continue
		}

		cls := classifyPath(path)
		l := langFor(path)

		// An extensionless file gets a blob read even though its guessed
		// grammar has no comments: it is usually a shell script, and the
		// shebang is the only thing that says so.
		scan := cls != classDocs && cls != classGenerated && (l.hasComments() || extOf(path) == "")

		var newMask, oldMask []lineKind
		if scan && len(f.Added) > 0 && f.NewPath != "" {
			if src, err := r.blob(headRev, f.NewPath); err == nil {
				l = langForContent(f.NewPath, src)
				if isGeneratedContent(src) {
					cls = classGenerated
				}
				newMask = scanFile(l, src)
			} else {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not read post-image (%v); comment detection for added lines falls back to text shape", f.NewPath, err))
			}
		}
		if scan && cls != classGenerated && len(f.Removed) > 0 && f.OldPath != "" {
			if src, err := r.blob(baseRev, f.OldPath); err == nil {
				oldMask = scanFile(langForContent(f.OldPath, src), src)
			} else {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not read pre-image (%v); comment detection for removed lines falls back to text shape", f.OldPath, err))
			}
		}

		if cls == classGenerated && !includeGenerated {
			res.Generated.Added += len(f.Added)
			res.Generated.Removed += len(f.Removed)
			fr.Class, fr.Excluded = classGenerated.String(), true
			fr.Buckets.Code = pair{Added: len(f.Added), Removed: len(f.Removed)}
			res.PerFile = append(res.PerFile, fr)
			continue
		}
		if cls == classGenerated {
			cls = classCode
		}
		fr.Class = cls.String()

		for _, cl := range f.Added {
			route(&fr.Buckets, &fr.Blank, cls, kindOf(newMask, cl, l), +1)
		}
		for _, cl := range f.Removed {
			route(&fr.Buckets, &fr.Blank, cls, kindOf(oldMask, cl, l), -1)
		}

		res.Buckets.merge(fr.Buckets)
		res.Blank.Added += fr.Blank.Added
		res.Blank.Removed += fr.Blank.Removed
		res.PerFile = append(res.PerFile, fr)
	}
	return res
}

// kindOf prefers the whole-file scan and falls back to the line's own text when
// the blob was unreadable. Blankness is read from the diff either way: it needs
// no context, and it stays right even when the scan is missing.
func kindOf(mask []lineKind, cl changedLine, l lang) lineKind {
	if isBlankText(cl.Text) {
		return lineBlank
	}
	if i := cl.No - 1; i >= 0 && i < len(mask) {
		return mask[i]
	}
	return classifyText(l, cl.Text)
}

func isBlankText(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isSpace(s[i]) {
			return false
		}
	}
	return true
}
