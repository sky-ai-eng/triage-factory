package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleConversations_Batched pins the aggregated run-list path the Board uses
// to collapse its per-refresh fan-out (TFAC-98): one
// GET /api/agent/conversations?task_ids=a,b&include=messages returns runs grouped per
// task (newest-first) plus each task's PRIMARY-run transcript keyed by run id.
func TestHandleConversations_Batched(t *testing.T) {
	s := newTestServer(t)

	// Task A: a primary (newest) run plus an older run on the same task.
	primaryA := seedSteerRun(t, s.db, "ba", "completed") // run r_ba on task t_ba, started_at≈now
	const taskA = "t_ba"
	const olderA = "r_ba_old"
	brOld := seedBlueprintRunSQLite(t, s.db, taskA)
	execSQL(t, s.db, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, started_at) VALUES (?, ?, 'p_ba', 'completed', 'manual', ?, '2020-01-01 00:00:00')`, olderA, taskA, brOld)

	// Task B: a single run.
	primaryB := seedSteerRun(t, s.db, "bb", "running") // run r_bb on task t_bb
	const taskB = "t_bb"

	store := sqlitestore.New(s.db)
	seedMsg := func(runID, content string) {
		if _, err := store.Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
			ConversationID: runID, Role: "assistant", Content: content, Subtype: "text",
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", runID, err)
		}
	}
	seedMsg(primaryA, "primary-a")
	seedMsg(olderA, "older-a") // must NOT appear — only the primary run's transcript is returned
	seedMsg(primaryB, "primary-b")

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations?task_ids="+taskA+","+taskB+"&include=messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET batched = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs     map[string][]map[string]any `json:"runs"`
		Messages map[string][]map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	// Runs grouped per task; task A has both runs, newest first.
	if len(resp.Runs[taskA]) != 2 {
		t.Fatalf("task A runs = %d, want 2", len(resp.Runs[taskA]))
	}
	if got, _ := resp.Runs[taskA][0]["ID"].(string); got != primaryA {
		t.Errorf("task A primary (runs[0]) = %q, want %q (newest first)", got, primaryA)
	}
	if len(resp.Runs[taskB]) != 1 {
		t.Fatalf("task B runs = %d, want 1", len(resp.Runs[taskB]))
	}

	// Messages: only the primary run per task, keyed by run id (the older run's
	// transcript is deliberately excluded — bounded payload, matches the
	// single-task path's latestRun).
	if _, ok := resp.Messages[olderA]; ok {
		t.Errorf("messages included older run %s; want only primary runs", olderA)
	}
	assertMsg := func(runID, want string) {
		ms := resp.Messages[runID]
		if len(ms) != 1 || ms[0]["content"] != want {
			t.Errorf("messages[%s] = %v, want one %q", runID, ms, want)
		}
	}
	assertMsg(primaryA, "primary-a")
	assertMsg(primaryB, "primary-b")

	// Without include=messages, the messages key is omitted entirely.
	rec2 := doJSON(t, s, http.MethodGet, "/api/agent/conversations?task_ids="+taskA, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET batched (no include) = %d, want 200", rec2.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec2.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["messages"]; ok {
		t.Errorf("messages key present without include=messages; want omitted")
	}
	if _, ok := raw["runs"]; !ok {
		t.Errorf("runs key missing")
	}
}

// TestHandleConversations_MissingParams pins that the run-list endpoint still
// requires a task selector — neither task_id nor task_ids is a 400, and an
// all-empty task_ids (commas/whitespace only) is rejected too.
func TestHandleConversations_MissingParams(t *testing.T) {
	s := newTestServer(t)

	if rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("GET runs (no params) = %d, want 400", rec.Code)
	}
	if rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations?task_ids=%20,%20", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("GET runs (empty task_ids) = %d, want 400", rec.Code)
	}
}

// TestHandleConversations_BatchedCap pins the task_ids cap: a request beyond
// maxBatchTaskIDs is truncated to the first N (not rejected), so a real task at
// the head still resolves while one placed past the cap is dropped.
func TestHandleConversations_BatchedCap(t *testing.T) {
	s := newTestServer(t)
	_ = seedSteerRun(t, s.db, "cap", "running") // run on task t_cap
	const taskID = "t_cap"

	pad := func(n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = uuid.New().String()
		}
		return ids
	}
	runsFor := func(ids []string) map[string][]map[string]any {
		rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations?task_ids="+strings.Join(ids, ","), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET batched (cap) = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Runs map[string][]map[string]any `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Runs
	}

	// Real task at the head, then > cap padding → kept after truncation.
	head := append([]string{taskID}, pad(maxBatchTaskIDs+50)...)
	if got := runsFor(head); len(got[taskID]) != 1 {
		t.Errorf("head within cap: task runs = %d, want 1", len(got[taskID]))
	}

	// Real task placed just past the cap (after maxBatchTaskIDs padding ids) →
	// truncated out, so it's absent from the response.
	tail := append(pad(maxBatchTaskIDs), taskID)
	if got := runsFor(tail); len(got[taskID]) != 0 {
		t.Errorf("tail beyond cap: task should be truncated out, got %d runs", len(got[taskID]))
	}
}
