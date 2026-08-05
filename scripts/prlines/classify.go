package main

import "strings"

// fileClass is the bucket a file's lines land in before per-line comment
// detection gets a say. The rules below are deliberately a plain table rather
// than anything clever: they encode this repo's layout, and the right response
// to a misfile is to edit the table, not to argue with a heuristic. Run with
// --explain to see what every changed file was classed as.
type fileClass int

const (
	classCode fileClass = iota
	classTests
	classDocs
	classGenerated
)

func (c fileClass) String() string {
	switch c {
	case classTests:
		return "tests"
	case classDocs:
		return "documentation"
	case classGenerated:
		return "generated"
	default:
		return "code"
	}
}

var (
	generatedBases = []string{
		"go.sum", "pnpm-lock.yaml", "package-lock.json", "yarn.lock",
		"bun.lock", "bun.lockb", "Cargo.lock",
	}
	generatedSuffixes = []string{
		".min.js", ".min.css", ".pb.go", "_pb.go", ".gen.go", "_gen.go",
		"_generated.go", ".snap",
	}
	generatedDirs = []string{"vendor", "node_modules", "dist", "third_party"}

	docExts  = []string{".md", ".mdx"}
	docBases = []string{"LICENSE", "NOTICE", "AUTHORS"}

	// Directories whose whole contents are test scaffolding. dbtest/pgtest are
	// this repo's shared store-conformance and testcontainer harnesses: they
	// hold no product behavior, so counting them as code would overstate every
	// PR that touches a store.
	testDirs = []string{
		"testdata", "__tests__", "__mocks__", "tests", "dbtest", "pgtest",
		"testutil", "testhelper", "testsupport",
	}
	testSuffixes = []string{"_test.go", "_test.rs", "_test.py"}
	testInfixes  = []string{".test.", ".spec."}
)

func classifyPath(p string) fileClass {
	base := baseName(p)
	ext := extOf(p)

	for _, b := range generatedBases {
		if base == b {
			return classGenerated
		}
	}
	for _, s := range generatedSuffixes {
		if strings.HasSuffix(base, s) {
			return classGenerated
		}
	}
	for _, d := range generatedDirs {
		if hasSegment(p, d) {
			return classGenerated
		}
	}

	// docs/ wins over everything below it, including the architecture specs
	// that happen to ship as .html and the Dockerfiles embedded in them.
	if strings.HasPrefix(p, "docs/") || hasSegment(p, "docs") {
		return classDocs
	}
	for _, e := range docExts {
		if ext == e {
			return classDocs
		}
	}
	for _, b := range docBases {
		if base == b || strings.HasPrefix(base, b+".") || strings.HasPrefix(base, b+"-") {
			return classDocs
		}
	}

	for _, s := range testSuffixes {
		if strings.HasSuffix(base, s) {
			return classTests
		}
	}
	for _, in := range testInfixes {
		if strings.Contains(base, in) {
			return classTests
		}
	}
	if strings.HasPrefix(base, "test_") && (ext == ".py" || ext == ".rs") {
		return classTests
	}
	for _, d := range testDirs {
		if hasSegment(p, d) {
			return classTests
		}
	}

	return classCode
}

// isGeneratedContent catches the files no path rule can know about, using Go's
// own convention for declaring a file machine-written.
func isGeneratedContent(src []byte) bool {
	// The marker is required to be in the first few lines of the file.
	head := src
	if len(head) > 4096 {
		head = head[:4096]
	}
	for i, line := range strings.Split(string(head), "\n") {
		if i > 10 {
			break
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "// Code generated ") && strings.HasSuffix(t, " DO NOT EDIT.") {
			return true
		}
	}
	return false
}

func hasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func extOf(p string) string {
	b := baseName(p)
	if i := strings.LastIndex(b, "."); i > 0 {
		return b[i:]
	}
	return ""
}
