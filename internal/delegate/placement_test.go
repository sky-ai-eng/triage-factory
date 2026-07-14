package delegate

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
)

// fakePlacement is a stub placementResolver: Enabled is fixed, and Resolve
// returns a plan whose single preferred owner is `owner` when the key value
// matches wantKey (so the test can assert the (org, repo) key derivation).
type fakePlacement struct {
	enabled bool
	wantKey string
	owner   string
	lastKey string
}

func (f *fakePlacement) Enabled() bool { return f.enabled }

func (f *fakePlacement) Resolve(_ context.Context, _, _, keyValue string) (placement.Plan, error) {
	f.lastKey = keyValue
	if keyValue != f.wantKey {
		return placement.Plan{Enabled: true}, nil
	}
	return placement.Plan{Enabled: true, PreferredSet: []string{f.owner}}, nil
}

func githubTask() domain.Task {
	return domain.Task{ID: "t1", EntitySource: "github", EntitySourceID: "acme/web#42"}
}

func TestPreferredExecutorFor_StampsRendezvousOwner(t *testing.T) {
	s := &Spawner{}
	fp := &fakePlacement{enabled: true, wantKey: "acme/web", owner: "exec-owner"}
	s.SetPlacement(fp, db.ClaimPlacement{Enabled: true})

	got := s.preferredExecutorFor(context.Background(), "org1", githubTask(), "run-1")
	if got != "exec-owner" {
		t.Fatalf("preferred = %q, want exec-owner", got)
	}
	if fp.lastKey != "acme/web" {
		t.Fatalf("resolver keyed on %q, want the owner/repo 'acme/web'", fp.lastKey)
	}
}

func TestPreferredExecutorFor_DisabledResolverStampsNothing(t *testing.T) {
	s := &Spawner{}
	s.SetPlacement(&fakePlacement{enabled: false, wantKey: "acme/web", owner: "exec-owner"}, db.ClaimPlacement{})
	if got := s.preferredExecutorFor(context.Background(), "org1", githubTask(), "run-1"); got != "" {
		t.Fatalf("disabled placement must stamp nothing, got %q", got)
	}
}

func TestPreferredExecutorFor_NilResolverStampsNothing(t *testing.T) {
	s := &Spawner{}
	if got := s.preferredExecutorFor(context.Background(), "org1", githubTask(), "run-1"); got != "" {
		t.Fatalf("nil resolver must stamp nothing, got %q", got)
	}
}

func TestPreferredExecutorFor_NonGitHubTaskStampsNothing(t *testing.T) {
	s := &Spawner{}
	s.SetPlacement(&fakePlacement{enabled: true, wantKey: "irrelevant", owner: "exec-owner"}, db.ClaimPlacement{Enabled: true})
	jira := domain.Task{ID: "t2", EntitySource: "jira", EntitySourceID: "SKY-123"}
	if got := s.preferredExecutorFor(context.Background(), "org1", jira, "run-1"); got != "" {
		t.Fatalf("a non-repo (Jira) task has no placement key, want empty, got %q", got)
	}
}

func TestClaimPlacement_DefaultsToDisabled(t *testing.T) {
	s := &Spawner{}
	if got := s.claimPlacement(); got.Enabled {
		t.Fatalf("unset claim placement must be disabled: %+v", got)
	}
}
