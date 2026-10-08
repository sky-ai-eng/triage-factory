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
// and a non-PR/issue provider resolve to nothing, a Jira target resolves only
// with its issue id and only on the org's current site, and the created row is
// a snapshot-less stub the poll cycle later enriches.
func TestResolveTouchedEntity_MapsProviderAndTarget(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	cases := []struct {
		name       string
		coord      entityCoordinate
		wantEntity bool
		wantSource string
		wantKind   string
	}{
		{
			name:       "github PR target → pr entity",
			coord:      entityCoordinate{provider: domain.ArtifactProviderGitHub, target: "octo/repo#18", url: "https://github.com/octo/repo/pull/18"},
			wantEntity: true, wantSource: "github", wantKind: "pr",
		},
		{
			name:       "jira issue target with its id → issue entity",
			coord:      entityCoordinate{provider: domain.ArtifactProviderJira, target: "SKY-123", externalID: "10123", url: "https://x/browse/SKY-123"},
			wantEntity: true, wantSource: "jira", wantKind: "issue",
		},
		{
			// A key alone names whichever issue holds it now; nothing is
			// resolved, or minted, on one.
			name:  "jira issue target without an id is skipped",
			coord: entityCoordinate{provider: domain.ArtifactProviderJira, target: "SKY-124"},
		},
		{
			// An artifact recorded on another site names an issue the org's
			// current site does not have.
			name:  "jira coordinate from another site is skipped",
			coord: entityCoordinate{provider: domain.ArtifactProviderJira, target: "SKY-125", externalID: "10125", scope: "https://other.example.com"},
		},
		{
			// A bare owner/repo target is repo-level — this is also a branch
			// push's shape (branchPushAction stamps Provider="github" with a
			// repo-level Target), so it covers that skip too.
			name:  "github repo-level target is skipped",
			coord: entityCoordinate{provider: domain.ArtifactProviderGitHub, target: "octo/repo"},
		},
		{
			// PR-shaped target that WOULD resolve under github — proves the skip
			// is on the unmapped provider (the default arm), not the target shape.
			name:  "unknown provider is skipped",
			coord: entityCoordinate{provider: "linear", target: "octo/repo#9"},
		},
		{
			name: "empty coordinate is a no-op",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := resolveTouchedEntityInfo(ctx, stores, info, tc.coord)
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
			ent, err := stores.Entities.GetBySource(ctx, org, tc.wantSource, testScope(tc.wantSource), tc.coord.target)
			if err != nil || ent == nil {
				t.Fatalf("GetBySource(%s, %s): ent=%v err=%v", tc.wantSource, tc.coord.target, ent, err)
			}
			if ent.ID != id {
				t.Errorf("resolved id %q != stored entity id %q", id, ent.ID)
			}
			if ent.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", ent.Kind, tc.wantKind)
			}
			if ent.ExternalID != tc.coord.externalID {
				t.Errorf("external_id = %q, want %q", ent.ExternalID, tc.coord.externalID)
			}
			// The resolver mints a stub; the poll cycle owns enrichment.
			if ent.SnapshotJSON != "" {
				t.Errorf("expected a snapshot-less stub, got snapshot %q", ent.SnapshotJSON)
			}
		})
	}

	// Idempotent: resolving an already-known target returns the same id and
	// creates no duplicate (octo/repo#18 was created by the first subtest).
	pr := entityCoordinate{provider: domain.ArtifactProviderGitHub, target: "octo/repo#18"}
	first, err := resolveTouchedEntityInfo(ctx, stores, info, pr)
	if err != nil {
		t.Fatalf("resolveTouchedEntity (idempotent): %v", err)
	}
	if first == "" {
		t.Fatal("expected the existing entity id, got empty")
	}
	again, _ := resolveTouchedEntityInfo(ctx, stores, info, pr)
	if again != first {
		t.Errorf("resolve not idempotent: first=%q again=%q", first, again)
	}
}

// TestResolveTouchedEntity_FollowsAMovedJiraIssue: a touch naming the key a
// Jira issue answers under now, with its id, resolves the issue's existing
// entity and renames it onto that key — never a second entity — and a touch by
// an id-less row's key learns the id.
func TestResolveTouchedEntity_FollowsAMovedJiraIssue(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	site := testScope("jira")

	moved, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "jira", site, "OLD-7", "10007", "issue", "Moved", site+"/browse/OLD-7")
	if err != nil {
		t.Fatal(err)
	}
	id, err := resolveTouchedEntityInfo(ctx, stores, info, entityCoordinate{provider: domain.ArtifactProviderJira, target: "NEW-7", externalID: "10007"})
	if err != nil || id != moved.ID {
		t.Fatalf("resolve = %q err=%v, want the existing entity %s", id, err, moved.ID)
	}
	got, _ := stores.Entities.GetSystem(ctx, org, moved.ID)
	if got.SourceID != "NEW-7" || got.URL != site+"/browse/NEW-7" {
		t.Errorf("entity = %+v, want it renamed onto NEW-7", got)
	}

	legacy, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "jira", site, "SKY-8", "", "issue", "Legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err = resolveTouchedEntityInfo(ctx, stores, info, entityCoordinate{provider: domain.ArtifactProviderJira, target: "SKY-8", externalID: "10008"})
	if err != nil || id != legacy.ID {
		t.Fatalf("resolve = %q err=%v, want the id-less row %s", id, err, legacy.ID)
	}
	if got, _ := stores.Entities.GetSystem(ctx, org, legacy.ID); got.ExternalID != "10008" {
		t.Errorf("entity = %+v, want it to learn 10008", got)
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

			ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", "https://jira.example.com", "SKY-1")
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

// testScope is the scope a test keys an entity of source under: what
// domain.EntityScope answers for an org with default settings where the
// source has one, and a fixed stand-in where it has none.
func testScope(source string) string {
	switch source {
	case "github":
		return "https://github.com"
	case "jira":
		return "https://jira.example.com"
	case "slack":
		return "slack.com"
	case "linear":
		return "ws-test"
	}
	return "test-scope"
}
