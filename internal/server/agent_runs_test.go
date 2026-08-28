package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestHandleConversations_Batched pins the aggregated conversation-list path the Board
// uses to collapse its per-refresh fan-out (TFAC-98): one
// POST /api/agent/conversations/list {task_ids, include_messages} returns the
// `runs` map grouped per task (newest-first) plus each task's PRIMARY
// conversation's transcript keyed by conversation id.
func TestHandleConversations_Batched(t *testing.T) {
	s := newTestServer(t)

	// Task A: a primary (newest) conversation plus an older one on the same task.
	primaryA := seedSteerConversation(t, s.db, "ba", "completed") // conversation r_ba on task t_ba, started_at≈now
	taskA := fixtureUUID("t_ba")
	olderA := fixtureUUID("r_ba_old")
	brOld := seedBlueprintRunSQLite(t, s.db, taskA)
	execSQL(t, s.db, `INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, blueprint_run_id, blueprint_step_index, started_at) VALUES (?, ?, ?, 'completed', 'manual', ?, 0, '2020-01-01 00:00:00')`, olderA, taskA, fixtureUUID("p_ba"), brOld)

	// Task B: a single conversation.
	primaryB := seedSteerConversation(t, s.db, "bb", "running") // run r_bb on task t_bb
	taskB := fixtureUUID("t_bb")

	store := sqlitestore.New(s.db)
	seedMsg := func(conversationID, content string) {
		if _, err := store.Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: content,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", conversationID, err)
		}
	}
	seedMsg(primaryA, "primary-a")
	seedMsg(olderA, "older-a") // must NOT appear — only the primary conversation's transcript is returned
	seedMsg(primaryB, "primary-b")

	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{
		"task_ids": []string{taskA, taskB}, "include_messages": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("batched list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs     map[string][]map[string]any `json:"runs"`
		Messages map[string][]map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	// Runs grouped per task; task A has both conversations, newest first.
	if len(resp.Runs[taskA]) != 2 {
		t.Fatalf("task A conversations = %d, want 2", len(resp.Runs[taskA]))
	}
	if got, _ := resp.Runs[taskA][0]["ID"].(string); got != primaryA {
		t.Errorf("task A primary (runs[0]) = %q, want %q (newest first)", got, primaryA)
	}
	if len(resp.Runs[taskB]) != 1 {
		t.Fatalf("task B conversations = %d, want 1", len(resp.Runs[taskB]))
	}

	// Messages: only the primary conversation per task, keyed by conversation id
	// (the older conversation's transcript is deliberately excluded — bounded
	// payload, matches the single-task path's latestConversation in Board.tsx).
	if _, ok := resp.Messages[olderA]; ok {
		t.Errorf("messages included older conversation %s; want only primary conversations", olderA)
	}
	assertMsg := func(conversationID, want string) {
		ms := resp.Messages[conversationID]
		if len(ms) != 1 || ms[0]["content"] != want {
			t.Errorf("messages[%s] = %v, want one %q", conversationID, ms, want)
		}
	}
	assertMsg(primaryA, "primary-a")
	assertMsg(primaryB, "primary-b")

	// Without include_messages, the messages key is omitted entirely.
	rec2 := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{"task_ids": []string{taskA}})
	if rec2.Code != http.StatusOK {
		t.Fatalf("batched list (no messages) = %d, want 200", rec2.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec2.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["messages"]; ok {
		t.Errorf("messages key present without include_messages; want omitted")
	}
	if _, ok := raw["runs"]; !ok {
		t.Errorf("runs key missing")
	}
}

// TestHandleConversations_TaskIDsOptional pins the two readings the selector
// has to keep apart. An ABSENT task_ids is no task narrowing — the whole
// visible set, which is what lets the rail ask the resource a question no task
// id can express. A task_ids that is PRESENT and unusable is still a 400: a
// corrupt filter must never widen the result set by falling back.
func TestHandleConversations_TaskIDsOptional(t *testing.T) {
	s := newTestServer(t)
	convA := seedSteerConversation(t, s.db, "opt-a", "completed")
	convB := seedSteerConversation(t, s.db, "opt-b", "running")

	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("list (no task_ids) = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs       map[string][]map[string]any `json:"runs"`
		TotalCount int                         `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, rows := range resp.Runs {
		for _, r := range rows {
			id, _ := r["ID"].(string)
			seen[id] = true
		}
	}
	if !seen[convA] || !seen[convB] {
		t.Errorf("unnarrowed list missed a conversation: got %v, want both %s and %s", seen, convA, convB)
	}
	if resp.TotalCount != 2 {
		t.Errorf("total_count = %d, want 2", resp.TotalCount)
	}

	if rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list",
		map[string]any{"task_ids": []string{" ", " "}}); rec.Code != http.StatusBadRequest {
		t.Errorf("list (blank task_ids) = %d, want 400", rec.Code)
	}
}

// TestHandleConversations_StatusFilter pins the rail's `running` read: the
// filter names DISPLAY statuses (a claim phase is a legal value), an unknown
// value is a client fault rather than an empty page, and a count-only page
// answers the number without a row.
func TestHandleConversations_StatusFilter(t *testing.T) {
	s := newTestServer(t)
	running := seedSteerConversation(t, s.db, "stf-run", "running")
	_ = seedSteerConversation(t, s.db, "stf-done", "completed")
	// `running` is DERIVED from an unreleased claim, never stored, so the
	// display ladder only reads it once the row has one.
	execSQL(t, s.db, `INSERT INTO claims (id, conversation_id, executor_id, boot_epoch) VALUES (?, ?, 'exec-1', 1)`,
		uuid.New().String(), running)

	live := append([]string{domain.StatusRunning}, domain.AllClaimPhases()...)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{
		"statuses": live, "page_size": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status-filtered count = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var count struct {
		Runs       map[string][]map[string]any `json:"runs"`
		TotalCount int                         `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if count.TotalCount != 1 {
		t.Errorf("total_count = %d, want 1 (only the claimed conversation is live)", count.TotalCount)
	}
	if len(count.Runs) != 0 {
		t.Errorf("count-only read returned %d groups of rows; want none", len(count.Runs))
	}

	// The stored column is not what gets filtered: the seeded row says
	// 'running' in the column too, but a row whose claim was never minted
	// reads as queued and must not be counted twice over.
	rec = doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{
		"statuses": []string{domain.StatusCompleted}, "page_size": 0,
	})
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if count.TotalCount != 1 {
		t.Errorf("completed total_count = %d, want 1", count.TotalCount)
	}

	rec = doJSON(t, s, http.MethodPost, "/api/agent/conversations/list",
		map[string]any{"statuses": []string{"nonsense"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonInvalidField, "statuses")
}

// TestHandleConversations_AttentionFilter pins the rail's `needs` read: a
// not-live conversation holding an unresolved artifact is one unit of
// attention, and a conversation holding nothing unresolved is none.
func TestHandleConversations_AttentionFilter(t *testing.T) {
	s := newTestServer(t)
	drafted := seedSteerConversation(t, s.db, "att-draft", "completed")
	_ = seedSteerConversation(t, s.db, "att-quiet", "completed")

	attention := func() int {
		t.Helper()
		rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list",
			map[string]any{"attention": true, "page_size": 0})
		if rec.Code != http.StatusOK {
			t.Fatalf("attention count = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			TotalCount int `json:"total_count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
		return resp.TotalCount
	}

	if got := attention(); got != 0 {
		t.Errorf("attention with nothing unresolved = %d, want 0", got)
	}
	// Two unresolved artifacts on one conversation are still one thing to do.
	for _, target := range []string{"o/r#1", "o/r#2"} {
		execSQL(t, s.db, `INSERT INTO artifacts (id, conversation_id, team_id, provider, kind, target, state, dedup_key)
			VALUES (?, ?, ?, 'github', 'pull_request', ?, 'draft', ?)`,
			uuid.New().String(), drafted, runmode.LocalDefaultTeamID, target, target)
	}
	if got := attention(); got != 1 {
		t.Errorf("attention counted per artifact, not per conversation: got %d, want 1", got)
	}
}

// TestHandleConversations_BatchedCap pins the task_ids cap: a request beyond
// maxBatchTaskIDs is REJECTED, not truncated. Truncation answered an oversized
// board with a board-shaped payload silently missing its tail — the caller had
// no way to tell the difference between "that task has no conversations" and "we
// stopped reading at 500".
func TestHandleConversations_BatchedCap(t *testing.T) {
	s := newTestServer(t)
	_ = seedSteerConversation(t, s.db, "cap", "running") // run on task t_cap
	taskID := fixtureUUID("t_cap")

	pad := func(n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = uuid.New().String()
		}
		return ids
	}

	// At the cap: still served, in full.
	within := append([]string{taskID}, pad(maxBatchTaskIDs-1)...)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{"task_ids": within})
	if rec.Code != http.StatusOK {
		t.Fatalf("batched list at cap = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs map[string][]map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs[taskID]) != 1 {
		t.Errorf("at cap: task conversations = %d, want 1", len(resp.Runs[taskID]))
	}

	// One past the cap: 400 OUT_OF_RANGE naming the field, nothing served.
	over := append([]string{taskID}, pad(maxBatchTaskIDs)...)
	rec = doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{"task_ids": over})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("batched list over cap = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonOutOfRange, "task_ids")
}

// TestHandleConversations_BatchedRejectsMalformedIDs: task_ids is a selector,
// so a value that can't be a task id fails the call rather than reaching a
// Postgres uuid column as a 22P02.
func TestHandleConversations_BatchedRejectsMalformedIDs(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list",
		map[string]any{"task_ids": []string{uuid.New().String(), "not-an-id"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonInvalidField, "task_ids")
}

// TestHandleConversations_RejectsUnknownField: the old `include=` free-text
// selector is gone — the body carries a typed include_messages bool, so a
// mistyped selector is an unknown FIELD the strict decoder refuses rather than
// a value the handler has to police. A typo can no longer return a payload
// missing the section the caller asked for.
func TestHandleConversations_RejectsUnknownField(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list", map[string]any{
		"task_ids": []string{uuid.New().String()}, "include": "messsages",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonUnknownField, "include")
}

// TestHandleMessages_SinceID pins the transcript read's optional watermark: the
// RunStation polls with it to repair rows whose websocket frame was dropped,
// and every other caller — which passes nothing — must keep getting the whole
// transcript. An unparseable or negative value is a 400: answering "give me
// what I'm missing" with the whole transcript is the silent widening a corrupt
// param must never buy.
func TestHandleMessages_SinceID(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "since", "running")

	store := sqlitestore.New(s.db)
	ids := map[string]int{}
	for _, content := range []string{"first", "second", "third"} {
		id, err := store.Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: content,
		})
		if err != nil {
			t.Fatalf("InsertMessage(%s): %v", content, err)
		}
		ids[content] = int(id)
	}

	contents := func(query string) []string {
		t.Helper()
		rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID+"/messages"+query, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET messages%s = %d; body=%s", query, rec.Code, rec.Body.String())
		}
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
		out := make([]string, 0, len(page.Items))
		for _, m := range page.Items {
			out = append(out, fmt.Sprint(m["content"]))
		}
		return out
	}
	eq := func(desc string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", desc, got, want)
		}
	}

	whole := []string{"first", "second", "third"}
	eq("no since_id", contents(""), whole)
	eq("since_id=0", contents("?since_id=0"), whole)
	eq("since_id=first", contents(fmt.Sprintf("?since_id=%d", ids["first"])), []string{"second", "third"})
	eq("since_id=third", contents(fmt.Sprintf("?since_id=%d", ids["third"])), nil)

	// Whitespace is trimmed before parsing, so a padded watermark is still a
	// watermark — treating it as unparseable would quietly turn a repair read
	// into a full transcript read on every tick.
	eq("padded since_id", contents(fmt.Sprintf("?since_id=%%20%d%%20", ids["first"])), []string{"second", "third"})

	// A watermark that isn't one fails the read rather than silently widening
	// it back to the whole transcript.
	for _, bad := range []string{"?since_id=nonsense", "?since_id=-5"} {
		rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID+"/messages"+bad, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET messages%s = %d, want 400; body=%s", bad, rec.Code, rec.Body.String())
		}
	}
}

// TestConversationResponse_TokenSums pins the four token rollups onto the run
// wire shape: the read already SUMs them per conversation, so the RunStation's
// telemetry rail reads the same authoritative numbers the usage dashboard
// does instead of re-summing the transcript client-side. Emitted as plain
// snake_case ints — a conversation with no usage-bearing message reads 0, never absent.
func TestConversationResponse_TokenSums(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "tok", "running")

	store := sqlitestore.New(s.db)
	usage := func(in, out, cacheRead, cacheWrite int) {
		t.Helper()
		if _, err := store.Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
			ConversationID: conversationID, Role: "assistant", Content: "working",
			InputTokens: &in, OutputTokens: &out, CacheReadTokens: &cacheRead, CacheCreationTokens: &cacheWrite,
		}); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
	}
	usage(10, 1, 100, 7)
	usage(20, 2, 200, 0)
	// A row with no usage at all (a user message / tool result) contributes
	// nothing rather than breaking the SUM.
	if _, err := store.Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: conversationID, Role: "user", Content: "carry on",
	}); err != nil {
		t.Fatalf("InsertMessage(no usage): %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	for key, want := range map[string]float64{
		"input_tokens":          30,
		"output_tokens":         3,
		"cache_read_tokens":     300,
		"cache_creation_tokens": 7,
	} {
		v, ok := got[key]
		if !ok {
			t.Errorf("conversation response missing %q", key)
			continue
		}
		if n, isNum := v.(float64); !isNum || n != want {
			t.Errorf("%s = %v, want %v", key, v, want)
		}
	}

	// A conversation that never streamed a usage-bearing message reads 0 on all four.
	bare := seedSteerConversation(t, s.db, "tokbare", "queued")
	rec = doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+bare, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET bare conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode bare: %v; body=%s", err, rec.Body.String())
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens"} {
		v, ok := got[key]
		if !ok {
			t.Errorf("bare conversation missing %q", key)
			continue
		}
		if n, isNum := v.(float64); !isNum || n != 0 {
			t.Errorf("bare conversation %s = %v, want 0", key, v)
		}
	}
}

// TestConversationResponse_CarriesOutcomeAndChainPosition pins the three fields a
// surface needs to tell a handed-off step from the one that ended the task:
// the terminal outcome, its abort note, and the owning blueprint's plan
// length beside the step's own index. Position is what disambiguates —
// `continue` is what an ordinary completion records, so the outcome alone
// cannot say whether the chain moves on.
func TestConversationResponse_CarriesOutcomeAndChainPosition(t *testing.T) {
	s := newTestServer(t)
	conversationID := seedSteerConversation(t, s.db, "chainpos", "running")

	// Step 0 of a three-step plan, concluded the way processCompletion does.
	var blueprintRunID string
	if err := s.db.QueryRow(`SELECT blueprint_run_id FROM conversations WHERE id = ?`, conversationID).Scan(&blueprintRunID); err != nil {
		t.Fatalf("read blueprint_run_id: %v", err)
	}
	execSQL(t, s.db, `UPDATE blueprint_runs SET step_plan = ? WHERE id = ?`,
		`[{"step_index":0},{"step_index":1},{"step_index":2}]`, blueprintRunID)
	execSQL(t, s.db, `UPDATE conversations SET blueprint_step_index = 0 WHERE id = ?`, conversationID)
	if _, err := sqlitestore.New(s.db).Conversations.CompleteSystem(context.Background(), runmode.LocalDefaultOrgID,
		conversationID, "completed", 0, 0, 0, "did my part", "continue", "", ""); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+conversationID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got["Outcome"] != "continue" {
		t.Errorf("Outcome = %v, want continue", got["Outcome"])
	}
	if _, ok := got["OutcomeReason"]; !ok {
		t.Error("conversation response omits OutcomeReason")
	}
	if n, isNum := got["blueprint_step_count"].(float64); !isNum || n != 3 {
		t.Errorf("blueprint_step_count = %v, want 3", got["blueprint_step_count"])
	}
	if n, isNum := got["blueprint_step_index"].(float64); !isNum || n != 0 {
		t.Errorf("blueprint_step_index = %v, want 0", got["blueprint_step_index"])
	}

	// The batched list path the board reads carries the same position, from
	// one lookup over the deduped blueprint_run ids.
	rec = doJSON(t, s, http.MethodPost, "/api/agent/conversations/list",
		map[string]any{"task_ids": []string{fixtureUUID("t_chainpos")}})
	if rec.Code != http.StatusOK {
		t.Fatalf("conversation list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Runs map[string][]map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, rec.Body.String())
	}
	convs := list.Runs[fixtureUUID("t_chainpos")]
	if len(convs) != 1 {
		t.Fatalf("listed conversations = %d, want 1", len(convs))
	}
	if n, isNum := convs[0]["blueprint_step_count"].(float64); !isNum || n != 3 {
		t.Errorf("listed blueprint_step_count = %v, want 3", convs[0]["blueprint_step_count"])
	}
	if convs[0]["Outcome"] != "continue" {
		t.Errorf("listed Outcome = %v, want continue", convs[0]["Outcome"])
	}

	// The single-prompt shape — every delegated run is a blueprint step, and a
	// plain one is a plan of exactly one, which is what says "no chain here".
	single := seedSteerConversation(t, s.db, "chainless", "queued")
	var singleBlueprintRunID string
	if err := s.db.QueryRow(`SELECT blueprint_run_id FROM conversations WHERE id = ?`, single).Scan(&singleBlueprintRunID); err != nil {
		t.Fatalf("read single blueprint_run_id: %v", err)
	}
	execSQL(t, s.db, `UPDATE blueprint_runs SET step_plan = ? WHERE id = ?`, `[{"step_index":0}]`, singleBlueprintRunID)
	rec = doJSON(t, s, http.MethodGet, "/api/agent/conversations/"+single, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET single-step conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode single-step: %v; body=%s", err, rec.Body.String())
	}
	if n, isNum := got["blueprint_step_count"].(float64); !isNum || n != 1 {
		t.Errorf("single-step blueprint_step_count = %v, want 1", got["blueprint_step_count"])
	}
}

// listConversations reads one task's conversations off the batched list route
// and unwraps the task-keyed map, so a test asserting on conversation rows
// doesn't restate the grouping every time.
func listConversations(t *testing.T, s *Server, taskID string) []map[string]any {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/agent/conversations/list",
		map[string]any{"task_ids": []string{taskID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("conversations list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs map[string][]map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode conversations list: %v; body=%s", err, rec.Body.String())
	}
	return resp.Runs[taskID]
}
