package delegate

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResolveActorAgentID pins the run-actor resolution rule the spawner stamps
// onto runs.actor_agent_id: prefer the task's live bot claim, fall back to the
// org agent, and degrade to "" (SQL NULL) when neither is available.
func TestResolveActorAgentID(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// The org's agent backs the fallback branch.
	orgAgentID, err := stores.Agents.Create(ctx, org, domain.Agent{DisplayName: "Org Bot"})
	if err != nil {
		t.Fatalf("Agents.Create: %v", err)
	}

	s := &Spawner{agents: stores.Agents}

	// Claim present → returned verbatim, even when it differs from the org agent
	// (the per-team claim is the precise actor; the spawner trusts it).
	if got := s.resolveActorAgentID(ctx, org, domain.Task{ID: "t-claim", ClaimedByAgentID: "claimed-agent"}); got != "claimed-agent" {
		t.Errorf("with claim: got %q, want %q (claim wins)", got, "claimed-agent")
	}

	// No claim → org-agent fallback (the auto-fire step-0 / swipe / post-takeover
	// case where the in-memory task carries no claim).
	if got := s.resolveActorAgentID(ctx, org, domain.Task{ID: "t-fallback"}); got != orgAgentID {
		t.Errorf("no claim: got %q, want org agent %q (fallback)", got, orgAgentID)
	}

	// Nil agents store (partial test wiring) → "" rather than a panic.
	sNil := &Spawner{}
	if got := sNil.resolveActorAgentID(ctx, org, domain.Task{ID: "t-nil"}); got != "" {
		t.Errorf("nil agents store: got %q, want \"\" (nil-safe)", got)
	}
}
