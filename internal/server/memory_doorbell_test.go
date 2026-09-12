package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// doorbell records every ring the server made, in order. Concurrency-safe
// because the settings and credential saves ring from the request goroutine
// while the archive cascade rings from its own.
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

// watchMemoryDoorbell wires the recorder in as the server's doorbell. Registered
// through the same setter internal/app uses, so a rename that skipped the
// server's own call sites would fail here.
func watchMemoryDoorbell(s *Server) *doorbell {
	d := &doorbell{}
	s.SetOnMemoryOwed(d.ring)
	return d
}

// seedLiveConversation puts one un-ended conversation on taskID and returns its
// id. Written as SQL for the same reason the other task fixtures are: what the
// boundary doors read is the row, and a store method that mints one would drag
// in a blueprint run this test says nothing about.
func seedLiveConversation(t *testing.T, s *Server, taskID string) string {
	t.Helper()
	id := uuid.New().String()
	execSQL(t, s.db, `
		INSERT INTO conversations (id, task_id, status, trigger_type, origin,
		                          team_id, visibility, creator_user_id)
		VALUES (?, ?, 'completed', 'manual', 'delegation', ?, 'team', ?)`,
		id, taskID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID)
	return id
}

func seedDoorbellTask(t *testing.T, s *Server) string {
	t.Helper()
	return seedTaskFixture(t, s.db, taskFixture{name: "doorbell", status: "queued"})
}

// Every conversation the task boundary stamps rings once, with its own id: the
// task's next delegation is held out of the claim gate until each of them has a
// memory row, so each is its own debt to settle.
func TestEndTaskConversations_RingsOncePerStampedConversation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	taskID := seedDoorbellTask(t, s)
	first := seedLiveConversation(t, s, taskID)
	second := seedLiveConversation(t, s, taskID)

	s.endTaskConversations(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, taskID, domain.EndedRequeued)

	got := d.taken()
	if len(got) != 2 {
		t.Fatalf("rang %d times for 2 stamped conversations: %v", len(got), got)
	}
	rung := map[string]bool{}
	for _, r := range got {
		if r[0] != runmode.LocalDefaultOrgID {
			t.Errorf("rang for org %q, want %q", r[0], runmode.LocalDefaultOrgID)
		}
		if rung[r[1]] {
			t.Errorf("rang twice for conversation %s", r[1])
		}
		rung[r[1]] = true
	}
	if !rung[first] || !rung[second] {
		t.Errorf("rings %v do not cover both stamped conversations (%s, %s)", got, first, second)
	}
}

// A boundary that stamped nothing rings nothing. Two shapes of that, and both
// matter: a second gesture on a task whose conversations have already ended
// (the door returns only the rows IT stamped, so the debt is not re-rung for),
// and a task that had nothing live at all.
func TestEndTaskConversations_RingsNothingWhenItStampedNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	taskID := seedDoorbellTask(t, s)
	seedLiveConversation(t, s, taskID)

	s.endTaskConversations(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, taskID, domain.EndedRequeued)
	if got := d.taken(); len(got) != 1 {
		t.Fatalf("first boundary rang %d times, want 1: %v", len(got), got)
	}

	// The already-ended re-ask: the first boundary is the one that happened.
	s.endTaskConversations(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, taskID, domain.EndedTakenOver)
	if got := d.taken(); len(got) != 0 {
		t.Errorf("a boundary over already-ended conversations rang %v", got)
	}

	// A task with nothing live on it at all.
	empty := seedDoorbellTask(t, s)
	s.endTaskConversations(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, empty, domain.EndedDelegated)
	if got := d.taken(); len(got) != 0 {
		t.Errorf("a boundary on a task with no live conversation rang %v", got)
	}
}

// The write is what the ring reports, not the request: the rows come back from
// the stamping statement itself, so a transaction that failed leaves nothing to
// ring for. Forced by asking a task id that names no row — the tx commits having
// stamped nothing, which is the same "nothing to ring" the rollback produces.
func TestEndTaskConversations_RingsNothingForAnUnknownTask(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	s.endTaskConversations(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultUserID, uuid.New().String(), domain.EndedRequeued)
	if got := d.taken(); len(got) != 0 {
		t.Errorf("a boundary that stamped nothing rang %v", got)
	}
}

// Choosing a background-jobs model is the fix for every generation that failed
// `no_model`, so the save re-kicks the org's whole backlog rather than letting
// each owed conversation age out of the attempt backoff. The org-wide form
// carries no conversation id.
func TestOrgSettingsPatch_BackgroundJobsModel_ReKicksTheMemorySweep(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	// Pick a model the org is not already on, so the save is a change.
	var model string
	for _, key := range modelcatalog.UniverseFor(false).Keys() {
		if key != orgBackgroundJobsModel(t, s) {
			model = key
			break
		}
	}
	if model == "" {
		t.Fatal("the local universe offers no second model to save")
	}

	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": model})
	got := d.taken()
	if len(got) != 1 {
		t.Fatalf("a model save rang %d times, want 1: %v", len(got), got)
	}
	if got[0] != [2]string{runmode.LocalDefaultOrgID, ""} {
		t.Errorf("rang %v, want the org-wide form (%q, \"\")", got[0], runmode.LocalDefaultOrgID)
	}
}

// A save that leaves the model where it was has fixed nothing, so it rings
// nothing — the re-kick is computed from what the save CHANGED. Same for
// clearing it: an org with no background-jobs model is an org whose generations
// will fail `no_model` again, and re-running the backlog to prove that is spend
// with no chance of landing.
func TestOrgSettingsPatch_BackgroundJobsModel_UnchangedAndClearedRingNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	current := orgBackgroundJobsModel(t, s)
	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": current})
	if got := d.taken(); len(got) != 0 {
		t.Errorf("re-sending the stored model rang %v", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"background_jobs_model": nil})
	if got := d.taken(); len(got) != 0 {
		t.Errorf("clearing the model rang %v", got)
	}

	// A save that changes something else entirely says nothing about the
	// backlog either.
	patchOrgSettingsOK(t, s, map[string]any{"max_concurrent_runs": 3})
	if got := d.taken(); len(got) != 0 {
		t.Errorf("an unrelated settings save rang %v", got)
	}
}

// Binding an LLM credential is the other fix for a `no_model` backlog — the
// model was named but its provider was not connected — so the credential PUTs
// ring the same org-wide doorbell. The bearer flavour stands for all three
// Bedrock shapes: they share bindBedrock, which is where the ring lives.
func TestBedrockBearerPut_ReKicksTheMemorySweep(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	path := "/api/orgs/" + runmode.LocalDefaultOrgID + "/llm/bedrock/bearer"
	rec := doJSON(t, s, http.MethodPut, path, map[string]any{
		"bearer_token": "test-bedrock-key",
		"region":       "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s: %d: %s", path, rec.Code, rec.Body.String())
	}
	got := d.taken()
	if len(got) != 1 {
		t.Fatalf("a Bedrock bind rang %d times, want 1: %v", len(got), got)
	}
	if got[0] != [2]string{runmode.LocalDefaultOrgID, ""} {
		t.Errorf("rang %v, want the org-wide form (%q, \"\")", got[0], runmode.LocalDefaultOrgID)
	}
}

// A refused save rings nothing: the credential never landed, so nothing the
// backlog was waiting on has changed.
func TestBedrockBearerPut_RefusedBindRingsNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	d := watchMemoryDoorbell(s)

	path := "/api/orgs/" + runmode.LocalDefaultOrgID + "/llm/bedrock/bearer"
	rec := doJSON(t, s, http.MethodPut, path, map[string]any{"region": "us-east-1"})
	if rec.Code == http.StatusOK {
		t.Fatalf("PUT with no bearer_token succeeded: %s", rec.Body.String())
	}
	if got := d.taken(); len(got) != 0 {
		t.Errorf("a refused bind rang %v", got)
	}
}
