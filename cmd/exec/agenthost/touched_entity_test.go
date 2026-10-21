package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
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
			name: "jira issue target with its id → issue entity",
			coord: entityCoordinate{provider: domain.ArtifactProviderJira, target: "SKY-123", externalID: "10123", url: "https://x/browse/SKY-123",
				container: domain.EntityContainer{JiraProject: domain.JiraProjectRef{ID: "10000", Key: "SKY"}}},
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

// TestResolveTouchedEntity_PRInAnUntrackedRepository: a touch on a pull
// request in a repository no team tracks creates its entity referencing a
// registry row for that repository, created with it and tracked by nobody.
func TestResolveTouchedEntity_PRInAnUntrackedRepository(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	id, err := resolveTouchedEntityInfo(ctx, stores, info, entityCoordinate{provider: domain.ArtifactProviderGitHub, target: "upstream/lib#3"})
	if err != nil || id == "" {
		t.Fatalf("resolveTouchedEntityInfo = %q, %v; want a new entity", id, err)
	}
	ent, err := stores.Entities.GetSystem(ctx, org, id)
	if err != nil || ent == nil {
		t.Fatalf("GetSystem(%s) = %v, %v", id, ent, err)
	}
	row, err := stores.Repos.GetByRefSystem(ctx, org, domain.RepoRef{Host: ent.Scope, Owner: "upstream", Repo: "lib"})
	if err != nil || row == nil {
		t.Fatalf("registry row for upstream/lib = %v, %v; want the one the touch created", row, err)
	}
	if ent.RepositoryID != row.ID {
		t.Errorf("entity references repository %q, want %s", ent.RepositoryID, row.ID)
	}
	tracked, err := stores.Repos.ListTrackedNamesSystem(ctx, org, ent.Scope)
	if err != nil {
		t.Fatalf("ListTrackedNamesSystem: %v", err)
	}
	for _, name := range tracked {
		if name == "upstream/lib" {
			t.Errorf("tracked names %v include upstream/lib; a touch tracks nothing", tracked)
		}
	}
}

// TestResolveTouchedEntity_FollowsAMovedJiraIssue: a touch naming the key a
// Jira issue answers under now, with its id, resolves the issue's existing
// entity and renames it onto that key — never a second entity — and a touch by
// the key of a row the SQLite migration carried over without an id learns the
// id.
func TestResolveTouchedEntity_FollowsAMovedJiraIssue(t *testing.T) {
	conn, stores, info := newCaptureStoresConn(t, true)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	site := testScope("jira")

	moved, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, org, site, "OLD-7", "10007", dbseed.JiraIssueProject("OLD-7"), "Moved", site+"/browse/OLD-7")
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

	legacy, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, org, site, "SKY-8", "10008", dbseed.JiraIssueProject("SKY-8"), "Legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	dbseed.ForgetIssueID(t, conn, legacy.ID)
	id, err = resolveTouchedEntityInfo(ctx, stores, info, entityCoordinate{provider: domain.ArtifactProviderJira, target: "SKY-8", externalID: "10008"})
	if err != nil || id != legacy.ID {
		t.Fatalf("resolve = %q err=%v, want the id-less row %s", id, err, legacy.ID)
	}
	if got, _ := stores.Entities.GetSystem(ctx, org, legacy.ID); got.ExternalID != "10008" {
		t.Errorf("entity = %+v, want it to learn 10008", got)
	}
}

// TestResolveTouchedEntity_FindsAPullRequestByItsNodeID: a touch carrying a
// pull request's node id resolves the entity that carries it under whatever
// key it is stored under, and leaves the key alone — a pull request's key
// moves with its repository's rename, and the touch may spell either name. A
// touch by an id-less row's key learns the id. The write funnel takes the id
// off a pull request artifact's node id, and links the artifact to the entity
// under that entity's key.
func TestResolveTouchedEntity_FindsAPullRequestByItsNodeID(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	host := testScope("github")

	stored, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", host, "octo/old#7", "PR_7", "pr", "Widget", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := resolveTouchedEntityInfo(ctx, stores, info, entityCoordinate{provider: domain.ArtifactProviderGitHub, target: "octo/new#7", externalID: "PR_7"})
	if err != nil || id != stored.ID {
		t.Fatalf("resolve = %q err=%v, want the existing entity %s", id, err, stored.ID)
	}
	if got, _ := stores.Entities.GetSystem(ctx, org, stored.ID); got.SourceID != "octo/old#7" {
		t.Errorf("entity key = %q, want it left at octo/old#7", got.SourceID)
	}
	if e, _ := stores.Entities.GetBySource(ctx, org, "github", host, "octo/new#7"); e != nil {
		t.Errorf("octo/new#7 = %+v, want no second entity", e)
	}

	stub, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", host, "octo/repo#8", "", "pr", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err = resolveTouchedEntityInfo(ctx, stores, info, entityCoordinate{provider: domain.ArtifactProviderGitHub, target: "octo/repo#8", externalID: "PR_8"})
	if err != nil || id != stub.ID {
		t.Fatalf("resolve = %q err=%v, want the stub %s", id, err, stub.ID)
	}
	if got, _ := stores.Entities.GetSystem(ctx, org, stub.ID); got.ExternalID != "PR_8" {
		t.Errorf("stub = %+v, want it to learn PR_8", got)
	}

	pr := domain.NewPullRequestArtifact("octo/new", 7, "PR_7", "feature", "main", "https://github.com/octo/new/pull/7", "Widget", "", false)
	if got := resolveArtifactEntity(ctx, stores, info, &pr, domain.EntityContainer{}); got.entityID != stored.ID {
		t.Errorf("funnel resolved %q, want the entity carrying the artifact's node id %s", got.entityID, stored.ID)
	}
	if pr.EntityID != stored.ID || pr.Target != "octo/old#7" {
		t.Errorf("artifact = entity %q target %q, want linked to %s under its key octo/old#7", pr.EntityID, pr.Target, stored.ID)
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

			ent, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "jira", jira.URL, "SKY-1")
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
