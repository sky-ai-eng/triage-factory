package domain

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// TestDeliverableStatesByKind_CoversEveryKind pins the lifecycle map to the kind
// vocabulary in both directions: a kind with no lifecycle makes its rows
// unfilterable by state, and a lifecycle for a kind nobody can name is dead.
func TestDeliverableStatesByKind_CoversEveryKind(t *testing.T) {
	for _, kind := range DeliverableKinds() {
		if len(deliverableStatesByKind[kind]) == 0 {
			t.Errorf("kind %q has no lifecycle in deliverableStatesByKind", kind)
		}
	}
	for kind := range deliverableStatesByKind {
		if !slices.Contains(DeliverableKinds(), kind) {
			t.Errorf("deliverableStatesByKind has %q, which DeliverableKinds does not name", kind)
		}
	}
}

// TestDeliverableStates_IsTheDedupedUnion pins the derivation: every state any
// kind declares is filterable, values that alias across kinds appear once, and
// the order is stable (it feeds the "want one of …" rejection message).
func TestDeliverableStates_IsTheDedupedUnion(t *testing.T) {
	got := DeliverableStates()

	for kind, states := range deliverableStatesByKind {
		for _, state := range states {
			if !slices.Contains(got, state) {
				t.Errorf("state %q (kind %q) is missing from DeliverableStates", state, kind)
			}
		}
	}
	for i, state := range got {
		if slices.Index(got, state) != i {
			t.Errorf("DeliverableStates repeats %q at index %d", state, i)
		}
	}
	if second := DeliverableStates(); !slices.Equal(got, second) {
		t.Errorf("DeliverableStates is not stable: %v then %v", got, second)
	}
}

// TestDeliverableStates_CoversEveryDeclaredConstant is the drift guard the
// hand-maintained union lacked. It reads the constants straight out of the
// source, so a new DeliverableState* that never reaches deliverableStatesByKind
// fails here rather than as a 400 on a filter the data actually produces —
// which is how 'posted' (comment, message) went missing.
func TestDeliverableStates_CoversEveryDeclaredConstant(t *testing.T) {
	src, err := os.ReadFile("deliverable.go")
	if err != nil {
		t.Fatalf("read deliverable.go: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^\s*(DeliverableState\w+)\s+=\s+"([^"]+)"`)
	matches := decl.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no DeliverableState* declarations — the scan pattern went stale")
	}
	states := DeliverableStates()
	for _, m := range matches {
		if !slices.Contains(states, m[2]) {
			t.Errorf("%s = %q is declared but absent from DeliverableStates", m[1], m[2])
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
