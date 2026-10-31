package domain

import (
	"slices"
	"testing"
)

// TestActionObjectKind_ClassifiesEveryAction holds the action vocabulary to
// the object kinds: every action either names a kind in ObjectKinds or is one
// of the actions that name none, so an action added without a decision about
// its object fails here.
func TestActionObjectKind_ClassifiesEveryAction(t *testing.T) {
	none := []string{ActionGHChannelWrite, ActionGraphQLWrite, ActionPRReverted, ActionRepoCreated}
	for _, action := range ExternalActionTypes() {
		kind := ActionObjectKind(action)
		switch {
		case IsRefusalOrFailure(action) || slices.Contains(none, action):
			if kind != "" {
				t.Errorf("%s names object kind %q, want none", action, kind)
			}
		case !slices.Contains(ObjectKinds(), kind):
			t.Errorf("%s names object kind %q, want one of %v", action, kind, ObjectKinds())
		}
	}
}
