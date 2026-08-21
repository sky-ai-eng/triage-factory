package eventsource

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

type fakePolicyStore struct {
	db.OrgEventSourceStore
	off  []string
	err  error
	orgs []string
}

func (f *fakePolicyStore) ListDisabledSystem(_ context.Context, orgID string) ([]string, error) {
	f.orgs = append(f.orgs, orgID)
	if f.err != nil {
		return nil, f.err
	}
	return f.off, nil
}

// TestDisabled_AnswersPerKindAndBindsTheOrg: the predicate every gate outside
// the availability derivation shares. It answers about one kind and reads the
// org it was handed — a gate that leaked another tenant's policy would be a
// cross-tenant off switch.
func TestDisabled_AnswersPerKindAndBindsTheOrg(t *testing.T) {
	store := &fakePolicyStore{off: []string{KindJira}}

	off, err := Disabled(context.Background(), store, "org-1", KindJira)
	if err != nil || !off {
		t.Fatalf("Disabled(jira) = %v, %v; want true, nil", off, err)
	}
	on, err := Disabled(context.Background(), store, "org-1", KindGitHub)
	if err != nil || on {
		t.Fatalf("Disabled(github) = %v, %v; want false, nil", on, err)
	}
	for _, got := range store.orgs {
		if got != "org-1" {
			t.Errorf("read org %q, want org-1", got)
		}
	}
}

// TestDisabled_PropagatesTheReadFailure: every caller fails closed on this, so
// the one thing it must never do is answer "not turned off" when it does not
// know.
func TestDisabled_PropagatesTheReadFailure(t *testing.T) {
	store := &fakePolicyStore{err: errors.New("boom")}

	off, err := Disabled(context.Background(), store, "org-1", KindJira)
	if err == nil {
		t.Fatal("Disabled returned nil error on a failed read")
	}
	if off {
		t.Error("Disabled reported true on a failed read; the caller must decide, not this")
	}
	if !strings.Contains(err.Error(), "org-1") {
		t.Errorf("error %q does not name the org whose policy could not be read", err)
	}
}

// TestDisabledError_NamesTheSourceAndWhatIsCovered. The wrapped sentinel is
// what lets a caller tell an admin's switch from an unbound credential without
// matching prose; the prose is what tells a reader — an agent, or a person in a
// log — that this covers more than the surface they hit.
func TestDisabledError_NamesTheSourceAndWhatIsCovered(t *testing.T) {
	err := DisabledError(KindJira)

	if !errors.Is(err, ErrDisabled) {
		t.Errorf("DisabledError does not wrap ErrDisabled: %v", err)
	}
	for _, want := range []string{"Jira", "polling", "agent access"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLabel(t *testing.T) {
	for kind, want := range map[string]string{
		KindGitHub: "GitHub",
		KindJira:   "Jira",
		"slack":    "Slack",
		"":         "This source",
	} {
		if got := Label(kind); got != want {
			t.Errorf("Label(%q) = %q, want %q", kind, got, want)
		}
	}
}
