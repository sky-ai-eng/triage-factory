package domain

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// TestExternalActionTypes_CoversEveryDeclaredConstant is the drift guard for
// the Actions lens's filter vocabulary, which is hand-listed (Go can't
// enumerate consts). It reads the constants straight out of the source, so a
// new Action* that never reaches ExternalActionTypes fails here rather than
// as a 400 on a filter the data actually produces.
func TestExternalActionTypes_CoversEveryDeclaredConstant(t *testing.T) {
	src, err := os.ReadFile("external_action.go")
	if err != nil {
		t.Fatalf("read external_action.go: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^\s*(Action\w+)\s+=\s+"([^"]+)"`)
	matches := decl.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no Action* declarations — the scan pattern went stale")
	}
	types := ExternalActionTypes()
	for _, m := range matches {
		if !slices.Contains(types, m[2]) {
			t.Errorf("%s = %q is declared but absent from ExternalActionTypes", m[1], m[2])
		}
	}
}
