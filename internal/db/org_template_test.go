package db_test

import (
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestOrgTemplate_UpdateHandler_MatchedSemantics pins the conditional-update
// contract that closes the PATCH-vs-promote race (SKY-381): UpdateHandler's
// WHERE pins the row's kind, so it reports matched=false — rather than
// silently no-op'ing — when the row was deleted or promoted (rule→trigger)
// since the caller read it. The handler relies on this to 404/409 instead of
// returning a misleading 200. Correct under READ COMMITTED on both dialects
// (a single conditional UPDATE re-checks its WHERE under a row lock).
func TestOrgTemplate_UpdateHandler_MatchedSemantics(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg

	if err := db.BootstrapNewOrg(ctx, stores, org, db.LocalDefaultTeamID, ai.ShippedPrompts()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	rules, err := stores.OrgTemplate.ListHandlers(ctx, org, domain.EventHandlerKindRule)
	if err != nil || len(rules) == 0 {
		t.Fatalf("ListHandlers(rule): err=%v n=%d", err, len(rules))
	}
	rule := rules[0]

	// A matching update reports matched=true.
	matched, err := stores.OrgTemplate.UpdateHandler(ctx, org, rule)
	if err != nil {
		t.Fatalf("UpdateHandler (matching rule): %v", err)
	}
	if !matched {
		t.Error("UpdateHandler on an existing rule reported matched=false; want true")
	}

	// A non-existent id reports matched=false, not an error.
	ghost := rule
	ghost.ID = "00000000-0000-0000-0000-0000000000ff"
	matched, err = stores.OrgTemplate.UpdateHandler(ctx, org, ghost)
	if err != nil {
		t.Fatalf("UpdateHandler (ghost id): %v", err)
	}
	if matched {
		t.Error("UpdateHandler on a non-existent id reported matched=true; want false")
	}

	// TOCTTOU: promote the rule to a trigger, then a stale rule-shaped update
	// must report matched=false (its kind-pinned WHERE no longer matches)
	// rather than silently clobbering the now-trigger row.
	prompts, err := stores.OrgTemplate.ListPrompts(ctx, org)
	if err != nil || len(prompts) == 0 {
		t.Fatalf("ListPrompts: err=%v n=%d", err, len(prompts))
	}
	bt := 3
	minA := 0.0
	if err := stores.OrgTemplate.PromoteHandler(ctx, org, rule.ID, domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            prompts[0].ID,
		BreakerThreshold:       &bt,
		MinAutonomySuitability: &minA,
	}); err != nil {
		t.Fatalf("PromoteHandler: %v", err)
	}
	matched, err = stores.OrgTemplate.UpdateHandler(ctx, org, rule) // stale rule shape
	if err != nil {
		t.Fatalf("UpdateHandler (stale rule after promote): %v", err)
	}
	if matched {
		t.Error("stale rule-shaped UpdateHandler after a concurrent promote reported matched=true; want false")
	}
}
