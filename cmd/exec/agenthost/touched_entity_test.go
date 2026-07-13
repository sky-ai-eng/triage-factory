package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResolveTouchedEntity_MapsProviderAndTarget pins the exec-touch resolver
// contract (TFAC-513 §2): ExternalAction.Provider → entity source,
// ExternalAction.Target → entity source_id, a repo-level GitHub target (no '#')
// and a non-PR/issue provider resolve to nothing, and the created row is a
// snapshot-less stub the poll cycle later enriches.
func TestResolveTouchedEntity_MapsProviderAndTarget(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	cases := []struct {
		name       string
		act        *domain.ExternalAction
		wantEntity bool
		wantSource string
		wantKind   string
	}{
		{
			name:       "github PR target → pr entity",
			act:        &domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Target: "octo/repo#18", URL: "https://github.com/octo/repo/pull/18"},
			wantEntity: true, wantSource: "github", wantKind: "pr",
		},
		{
			name:       "jira issue target → issue entity",
			act:        &domain.ExternalAction{Provider: domain.ArtifactProviderJira, Target: "SKY-123", URL: "https://x/browse/SKY-123"},
			wantEntity: true, wantSource: "jira", wantKind: "issue",
		},
		{
			// A bare owner/repo target is repo-level — this is also a branch
			// push's shape (branchPushAction stamps Provider="github" with a
			// repo-level Target), so it covers that skip too.
			name: "github repo-level target is skipped",
			act:  &domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Target: "octo/repo"},
		},
		{
			// PR-shaped target that WOULD resolve under github — proves the skip
			// is on the unmapped provider (the default arm), not the target shape.
			name: "unknown provider is skipped",
			act:  &domain.ExternalAction{Provider: "linear", Target: "octo/repo#9"},
		},
		{
			name: "nil action is a no-op",
			act:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := resolveTouchedEntityInfo(ctx, stores, info, tc.act)
			if err != nil {
				t.Fatalf("resolveTouchedEntity: %v", err)
			}
			if !tc.wantEntity {
				if id != "" {
					t.Fatalf("expected no entity, got id=%q", id)
				}
				return
			}
			if id == "" {
				t.Fatal("expected an entity id, got empty")
			}
			ent, err := stores.Entities.GetBySource(ctx, org, tc.wantSource, tc.act.Target)
			if err != nil || ent == nil {
				t.Fatalf("GetBySource(%s, %s): ent=%v err=%v", tc.wantSource, tc.act.Target, ent, err)
			}
			if ent.ID != id {
				t.Errorf("resolved id %q != stored entity id %q", id, ent.ID)
			}
			if ent.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", ent.Kind, tc.wantKind)
			}
			// The resolver mints a stub; the poll cycle owns enrichment.
			if ent.SnapshotJSON != "" {
				t.Errorf("expected a snapshot-less stub, got snapshot %q", ent.SnapshotJSON)
			}
		})
	}

	// Idempotent: resolving an already-known target returns the same id and
	// creates no duplicate (octo/repo#18 was created by the first subtest).
	first, err := resolveTouchedEntityInfo(ctx, stores, info, &domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Target: "octo/repo#18"})
	if err != nil {
		t.Fatalf("resolveTouchedEntity (idempotent): %v", err)
	}
	if first == "" {
		t.Fatal("expected the existing entity id, got empty")
	}
	again, _ := resolveTouchedEntityInfo(ctx, stores, info, &domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Target: "octo/repo#18"})
	if again != first {
		t.Errorf("resolve not idempotent: first=%q again=%q", first, again)
	}
}

// TestRecordTouch_LiveWiring_CreatesEntityOnOrgWrite proves the resolver is wired
// into the live recording funnels (TFAC-513 §2): a bot Jira write resolves-or-
// creates the touched entity as a side effect of recording the action, even
// before TFAC-507's touched-set is in place.
func TestRecordTouch_LiveWiring_CreatesEntityOnOrgWrite(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			jira := startFakeJira(t)
			stores, info := newJiraRecordingStoresForCapture(t, jira.URL, eventTriggered)
			client := NewLocal(stores, info)
			ctx := context.Background()

			if _, err := client.JiraCreateIssue(ctx, "SKY", "Task", "do a thing", "", "", ""); err != nil {
				t.Fatalf("JiraCreateIssue: %v", err)
			}

			ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", "SKY-1")
			if err != nil {
				t.Fatalf("GetBySource: %v", err)
			}
			if ent == nil {
				t.Fatal("touched entity was not resolved on the org-credential Jira write")
			}
			if ent.Kind != "issue" || ent.SnapshotJSON != "" {
				t.Errorf("unexpected touched entity: %+v", ent)
			}
		})
	}
}
