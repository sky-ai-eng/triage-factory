// The doorbell the executor's boundary stamps ring: a conversation that ended
// without its agent having written its memory owes one, and the brain is what
// generates it. The executor is never the brain, so every ring here is a relay.

package delegate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// doorbell records what the spawner rang for.
type doorbell struct {
	mu    sync.Mutex
	rings [][2]string
}

func (d *doorbell) ring(orgID, conversationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rings = append(d.rings, [2]string{orgID, conversationID})
}

func (d *doorbell) taken() [][2]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.rings
	d.rings = nil
	return out
}

// assertRangOnceFor is the whole assertion in one place: exactly one ring, for
// this org and this conversation. Twice would mean two generations of the same
// memory; none would mean the debt waits out a sweep interval.
func (d *doorbell) assertRangOnceFor(t *testing.T, orgID, conversationID string) {
	t.Helper()
	got := d.taken()
	if len(got) != 1 {
		t.Fatalf("rang %d times, want exactly 1: %v", len(got), got)
	}
	if got[0] != [2]string{orgID, conversationID} {
		t.Errorf("rang %v, want (%q, %q)", got[0], orgID, conversationID)
	}
}

// A step advance ends the step it moves past, and the next step is held out of
// the claim gate until that step's memory lands — so the ring is what keeps a
// blueprint's pause between steps to a generation rather than a sweep interval.
func TestReactor_AdvanceRingsTheMemoryDoorbell(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	s, _, brID, _, step0 := reactorFixture(t, "adv-doorbell", 2, "completed", "continue")
	d := &doorbell{}
	s.SetOnMemoryOwed(d.ring)

	stepConversation := loadConversation(t, s, step0)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	d.assertRangOnceFor(t, org, step0)
}

// A failure is a boundary, and the memory it owes is the one that says how far
// the work got — anywhere between "before the first tool call" and "one write
// short of the file". Only the transcript knows, which is what the brain reads.
func TestFailConversation_RingsTheMemoryDoorbell(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	const conversationID = "r-fail-doorbell"
	seedConversation(t, database, conversationID, "sess", t.TempDir())
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	d := &doorbell{}
	s.SetOnMemoryOwed(d.ring)

	if fenced := s.failConversation(org, conversationID, "", "", "manual", runmode.LocalDefaultUserID,
		"the runtime died", domain.ConversationFailureUnclassified); fenced {
		t.Fatal("failConversation reported fenced; nothing holds a claim on this fixture")
	}
	d.assertRangOnceFor(t, org, conversationID)

	// A second failure on the same conversation stamps nothing — the first
	// boundary is the one that happened — so it rings nothing either.
	if fenced := s.failConversation(org, conversationID, "", "", "manual", runmode.LocalDefaultUserID,
		"and again", domain.ConversationFailureUnclassified); fenced {
		t.Fatal("failConversation reported fenced on the second call")
	}
	if got := d.taken(); len(got) != 0 {
		t.Errorf("a failure over an already-ended conversation rang %v", got)
	}
}

// The orphaned-conversation disposition: a step claimed and never run, whose
// blueprint vanished under it. It owes the memory that says the work never
// started — the mechanical empty row, which the brain writes without spending a
// model call, and which still has to exist before the task opens anything new.
func TestFailClaimedConversation_RingsTheMemoryDoorbell(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	s, database, _, _, step0 := reactorFixture(t, "claimed-doorbell", 2, "completed", "continue")
	d := &doorbell{}
	s.SetOnMemoryOwed(d.ring)

	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, step0); err != nil {
		t.Fatalf("force the step mid-flight: %v", err)
	}
	claimed, err := s.conversationQueue.ClaimNextConversation(context.Background(), "exec-doorbell", 1, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != step0 {
		t.Fatalf("claimed = %v, want the seeded step %s", claimed, step0)
	}

	s.failClaimedConversation(org, claimed, "load task: gone")
	d.assertRangOnceFor(t, org, step0)
}

// No doorbell wired is the default, every test's shape, and survivable in
// production: a boundary still stamps its row, and the brain's backstop sweep
// settles the debt one interval later. The doorbell only ever shortens a wait.
func TestBoundaries_RingNothingWithNoDoorbellWired(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	const conversationID = "r-no-doorbell"
	seedConversation(t, database, conversationID, "sess", t.TempDir())
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if fenced := s.failConversation(org, conversationID, "", "", "manual", runmode.LocalDefaultUserID,
		"the runtime died", domain.ConversationFailureUnclassified); fenced {
		t.Fatal("failConversation reported fenced")
	}
	assertEnded(t, database, conversationID, domain.EndedFailed)
}
