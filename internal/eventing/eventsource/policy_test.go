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

// TestDisableable_GitHubIsRequired is the rule itself: the vocabulary an org
// can turn off is a strict subset of the one it can read a state for.
func TestDisableable_GitHubIsRequired(t *testing.T) {
	if Disableable(KindGitHub) {
		t.Error("github is disableable; an org with github off is an org where nothing works")
	}
	if !Declared(KindGitHub) {
		t.Error("github stopped being declared; it must still resolve a state and appear in the availability read")
	}
	if !Disableable(KindJira) {
		t.Error("jira is not disableable; it is the source this whole switch exists for")
	}
	if Disableable("gerrit") {
		t.Error("an undeclared kind is disableable; there is nothing there to turn off")
	}
}

// TestDisabled_RequiredSourceAnswersFalseWithoutReading pins the short-circuit
// that carries the rule to every gate. The store here would say github is off;
// the point is that nobody asks it, so a hand-written row — the column is
// FK-free by design, so nothing at the schema level forbids one — cannot take
// the product down.
func TestDisabled_RequiredSourceAnswersFalseWithoutReading(t *testing.T) {
	store := &fakePolicyStore{off: []string{KindGitHub}}

	off, err := Disabled(context.Background(), store, "org-1", KindGitHub)
	if err != nil || off {
		t.Fatalf("Disabled(github) = %v, %v; want false, nil", off, err)
	}
	if len(store.orgs) != 0 {
		t.Errorf("read the store %d times for a source with no off switch, want 0", len(store.orgs))
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

// TestResolverGate_RefusesATurnedOffSourceOnEveryCall: the check a Jira or
// Linear credential resolver runs before every resolution. Off refuses with
// ErrDisabled; on lets the resolution through; a failed read refuses with the
// read's own error, never ErrDisabled, which would send the reader to an admin
// about a database fault. It reads the policy on every call, so turning the
// source back on takes effect on the next resolution.
func TestResolverGate_RefusesATurnedOffSourceOnEveryCall(t *testing.T) {
	ctx := context.Background()
	store := &fakePolicyStore{off: []string{KindJira}}
	gate := ResolverGate(store, KindJira)

	if err := gate(ctx, "org-1"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("turned off: gate = %v, want ErrDisabled", err)
	}
	store.off = nil
	if err := gate(ctx, "org-1"); err != nil {
		t.Fatalf("turned back on: gate = %v, want nil", err)
	}
	store.err = errors.New("boom")
	if err := gate(ctx, "org-1"); err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("unreadable policy: gate = %v, want the read's error", err)
	}
	if len(store.orgs) != 3 {
		t.Errorf("read the policy %d times over three resolutions, want 3: a cached answer keeps a turned-off source answering", len(store.orgs))
	}
	for _, got := range store.orgs {
		if got != "org-1" {
			t.Errorf("read org %q, want org-1", got)
		}
	}
}
