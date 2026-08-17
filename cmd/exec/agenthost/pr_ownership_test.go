package agenthost

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// prOwnershipFixture is the shared setup for the stamp tests: capture stores
// wired to an in-memory DB, plus a second team to play the part of an owner
// this path must not be able to displace.
func prOwnershipFixture(t *testing.T) (*sql.DB, db.Stores, ConversationInfo, string) {
	t.Helper()
	conn, stores, info := newCaptureStoresConn(t, true)
	const otherTeam = "22222222-2222-2222-2222-222222222222"
	if _, err := conn.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'other', 'Other')`,
		otherTeam, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed second team: %v", err)
	}
	return conn, stores, info, otherTeam
}

// botOpenedPR is the artifact the recording funnel receives when the agent
// opens a PR — the same shape both write paths build (the exec verb via
// recordGithubPR, the gh channel via ObservationArtifact).
func botOpenedPR() domain.Artifact {
	return domain.NewPullRequestArtifact(
		"octo/repo", 42, "PR_kwDOABCD", "feature/x", "main",
		"https://github.com/octo/repo/pull/42", "Fix the thing", "Body", true,
	)
}

func ownerOf(t *testing.T, stores db.Stores, entityID string) string {
	t.Helper()
	team, err := stores.Entities.OwningTeamForEntitySystem(
		context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("OwningTeamForEntitySystem: %v", err)
	}
	return team
}

func prEntity(t *testing.T, stores db.Stores) *domain.Entity {
	t.Helper()
	ent, err := stores.Entities.GetBySource(
		context.Background(), runmode.LocalDefaultOrgID, "github", "octo/repo#42")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	return ent
}

// TestStampPROwnership_ArtifactFirst covers the ordinary ordering: the run
// opens the PR before any poll has seen it. The funnel mints the entity on the
// PR's natural key and stamps the commissioning team, so the PR is owned before
// its first CI event exists — which is the whole point, since the PR's author
// is a bot that resolves to no TF user.
func TestStampPROwnership_ArtifactFirst(t *testing.T) {
	_, stores, info, _ := prOwnershipFixture(t)
	a := botOpenedPR()

	RecordExternalWrite(context.Background(), stores, info, &a, nil)

	ent := prEntity(t, stores)
	if ent == nil {
		t.Fatal("no entity minted for the opened PR")
	}
	if got := ownerOf(t, stores, ent.ID); got != info.TeamID {
		t.Errorf("owning team = %q, want the run's team %q", got, info.TeamID)
	}
}

// TestStampPROwnership_EntityFirst covers the reverse ordering: a poll cycle
// discovered the PR before the run's artifact write landed. The stamp has to
// back-fill the row that already exists rather than needing to create it —
// otherwise the ownership signal is lost to a race the caller cannot control.
func TestStampPROwnership_EntityFirst(t *testing.T) {
	ctx := context.Background()
	_, stores, info, _ := prOwnershipFixture(t)

	// Stand in for the poller: same natural key, no owner.
	discovered, created, err := stores.Entities.FindOrCreateSystem(
		ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#42", "pr", "Fix the thing", "")
	if err != nil || !created {
		t.Fatalf("seed discovered entity: created=%v err=%v", created, err)
	}
	if got := ownerOf(t, stores, discovered.ID); got != "" {
		t.Fatalf("discovered entity started owned by %q", got)
	}

	a := botOpenedPR()
	RecordExternalWrite(ctx, stores, info, &a, nil)

	if ent := prEntity(t, stores); ent == nil || ent.ID != discovered.ID {
		t.Fatalf("stamp created a second entity instead of back-filling %q", discovered.ID)
	}
	if got := ownerOf(t, stores, discovered.ID); got != info.TeamID {
		t.Errorf("owning team = %q, want the run's team %q", got, info.TeamID)
	}
}

// TestStampPROwnership_DoesNotOverwriteExistingOwner pins the stamp-if-NULL
// half. An entity transferred to another team keeps that owner: this path
// records an intent, it does not arbitrate between owners.
func TestStampPROwnership_DoesNotOverwriteExistingOwner(t *testing.T) {
	ctx := context.Background()
	_, stores, info, otherTeam := prOwnershipFixture(t)

	ent, _, err := stores.Entities.FindOrCreateSystem(
		ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#42", "pr", "Fix the thing", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := stores.Entities.StampOwningTeamIfUnsetSystem(
		ctx, runmode.LocalDefaultOrgID, ent.ID, otherTeam); err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	a := botOpenedPR()
	RecordExternalWrite(ctx, stores, info, &a, nil)

	if got := ownerOf(t, stores, ent.ID); got != otherTeam {
		t.Errorf("owning team = %q, want the pre-existing owner %q", got, otherTeam)
	}
}

// TestStampPROwnership_NoTeamOnRun is the defensive case. Auto-delegation is
// gated on an owned task, so a teamless run should not reach the funnel — but
// if one does there is no owner to record, and it must leave the column NULL
// for a later writer rather than erroring or stamping something empty.
func TestStampPROwnership_NoTeamOnRun(t *testing.T) {
	ctx := context.Background()
	_, stores, info, _ := prOwnershipFixture(t)

	ent, _, err := stores.Entities.FindOrCreateSystem(
		ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#42", "pr", "Fix the thing", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	info.TeamID = ""
	a := botOpenedPR()
	RecordExternalWrite(ctx, stores, info, &a, nil)

	if got := ownerOf(t, stores, ent.ID); got != "" {
		t.Errorf("teamless run stamped an owner: %q", got)
	}
}

// TestStampPROwnership_WithAction covers the exec-verb path, where the funnel
// receives the artifact AND its audit action together. githubAction copies the
// artifact's target and url, so both the touch pass and the stamp want the same
// entity; the stamp reuses what the touch resolved rather than running a second
// FindOrCreate. Both effects still have to land.
func TestStampPROwnership_WithAction(t *testing.T) {
	ctx := context.Background()
	conn, stores, info, _ := prOwnershipFixture(t)

	a := botOpenedPR()
	act := &domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub,
		Action:   domain.ActionPRCreated,
		Target:   a.Target,
		URL:      a.URL,
	}
	RecordExternalWrite(ctx, stores, info, &a, act)

	ent := prEntity(t, stores)
	if ent == nil {
		t.Fatal("no entity for the opened PR")
	}
	if got := ownerOf(t, stores, ent.ID); got != info.TeamID {
		t.Errorf("owning team = %q, want %q", got, info.TeamID)
	}
	// The reuse must not have cost the touch row the action pass writes.
	var touches int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ? AND entity_id = ? AND role = 'touched'`,
		info.ConversationID, ent.ID,
	).Scan(&touches); err != nil {
		t.Fatalf("count touches: %v", err)
	}
	if touches == 0 {
		t.Error("reusing the touch's resolution lost the touch row")
	}
}

// TestStampPROwnership_ActionTargetsElsewhere is why the reuse is guarded on the
// coordinates rather than assuming the pair always agree. If an action ever
// names a different object than the artifact beside it, ownership must follow
// the ARTIFACT — that is the PR that was opened — and the touch must still
// follow the action.
func TestStampPROwnership_ActionTargetsElsewhere(t *testing.T) {
	ctx := context.Background()
	_, stores, info, _ := prOwnershipFixture(t)

	a := botOpenedPR() // octo/repo#42
	act := &domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub,
		Action:   domain.ActionPRCreated,
		Target:   "octo/repo#7",
		URL:      "https://github.com/octo/repo/pull/7",
	}
	RecordExternalWrite(ctx, stores, info, &a, act)

	owned := prEntity(t, stores)
	if owned == nil {
		t.Fatal("no entity for the artifact's PR")
	}
	if got := ownerOf(t, stores, owned.ID); got != info.TeamID {
		t.Errorf("artifact's PR owner = %q, want %q", got, info.TeamID)
	}

	other, err := stores.Entities.GetBySource(ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#7")
	if err != nil || other == nil {
		t.Fatalf("the action's own entity was not resolved: ent=%v err=%v", other, err)
	}
	if got := ownerOf(t, stores, other.ID); got != "" {
		t.Errorf("the action's PR was stamped %q; ownership must follow the artifact", got)
	}
}

// TestStampPROwnership_ReviewArtifactDoesNotClaimOwnership guards the narrowest
// and most consequential edge: a submitted review carries the same
// owner/repo#N target as the PR it reviews. Reviewing another team's PR must
// never transfer that PR to the reviewing team, so the stamp keys on the
// artifact kind, not on the target shape.
func TestStampPROwnership_ReviewArtifactDoesNotClaimOwnership(t *testing.T) {
	ctx := context.Background()
	_, stores, info, _ := prOwnershipFixture(t)

	ent, _, err := stores.Entities.FindOrCreateSystem(
		ctx, runmode.LocalDefaultOrgID, "github", "octo/repo#42", "pr", "Someone else's PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	a := domain.NewSubmittedReviewArtifact(
		"octo/repo", 42, 9001, "APPROVE",
		"https://github.com/octo/repo/pull/42#pullrequestreview-9001", info.ConversationID)
	// Same owner/repo#N target the PR artifact carries — the kind is the only
	// thing separating them, which is exactly what this asserts.
	if a.Target != "octo/repo#42" {
		t.Fatalf("review target = %q, want the PR's natural key", a.Target)
	}
	RecordExternalWrite(ctx, stores, info, &a, nil)

	if got := ownerOf(t, stores, ent.ID); got != "" {
		t.Errorf("a review claimed ownership of the reviewed PR: %q", got)
	}
}
