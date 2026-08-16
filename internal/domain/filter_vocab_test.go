package domain

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// TestArtifactStatesByKind_CoversEveryKind pins the lifecycle map to the kind
// vocabulary in both directions: a kind with no lifecycle makes its rows
// unfilterable by state, and a lifecycle for a kind nobody can name is dead.
func TestArtifactStatesByKind_CoversEveryKind(t *testing.T) {
	for _, kind := range ArtifactKinds() {
		if len(artifactStatesByKind[kind]) == 0 {
			t.Errorf("kind %q has no lifecycle in artifactStatesByKind", kind)
		}
	}
	for kind := range artifactStatesByKind {
		if !slices.Contains(ArtifactKinds(), kind) {
			t.Errorf("artifactStatesByKind has %q, which ArtifactKinds does not name", kind)
		}
	}
}

// TestArtifactStates_IsTheDedupedUnion pins the derivation: every state any
// kind declares is filterable, values that alias across kinds appear once, and
// the order is stable (it feeds the "want one of …" rejection message).
func TestArtifactStates_IsTheDedupedUnion(t *testing.T) {
	got := ArtifactStates()

	for kind, states := range artifactStatesByKind {
		for _, state := range states {
			if !slices.Contains(got, state) {
				t.Errorf("state %q (kind %q) is missing from ArtifactStates", state, kind)
			}
		}
	}
	for i, state := range got {
		if slices.Index(got, state) != i {
			t.Errorf("ArtifactStates repeats %q at index %d", state, i)
		}
	}
	if second := ArtifactStates(); !slices.Equal(got, second) {
		t.Errorf("ArtifactStates is not stable: %v then %v", got, second)
	}
}

// TestArtifactStates_CoversEveryDeclaredConstant is the drift guard the
// hand-maintained union lacked. It reads the constants straight out of the
// source, so a new ArtifactState* that never reaches artifactStatesByKind
// fails here rather than as a 400 on a filter the data actually produces —
// which is how 'posted' (comment, message) went missing.
func TestArtifactStates_CoversEveryDeclaredConstant(t *testing.T) {
	src, err := os.ReadFile("artifact.go")
	if err != nil {
		t.Fatalf("read artifact.go: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^\s*(ArtifactState\w+)\s+=\s+"([^"]+)"`)
	matches := decl.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no ArtifactState* declarations — the scan pattern went stale")
	}
	states := ArtifactStates()
	for _, m := range matches {
		if !slices.Contains(states, m[2]) {
			t.Errorf("%s = %q is declared but absent from ArtifactStates", m[1], m[2])
		}
	}
}

// TestExternalActionTypes_CoversEveryDeclaredConstant is the same guard for the
// Actions lens's filter vocabulary, which is hand-listed for the same reason
// (Go can't enumerate consts) and carries the same drift risk.
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
