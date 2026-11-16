package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// seedOneStepBlueprint seeds a one-step blueprint on the local team whose
// prompt pins model, "" for a step that inherits the team default, and
// answers its id.
func seedOneStepBlueprint(t *testing.T, s *Server, tag, model string) string {
	t.Helper()
	promptID, blueprintID := "p-"+tag, "bp-"+tag
	execSQL(t, s.db,
		`INSERT INTO prompts (id, name, body, model, source, creator_user_id) VALUES (?, ?, 'do the thing', ?, 'user', ?)`,
		promptID, tag, model, runmode.LocalDefaultUserID)
	if _, err := s.blueprints.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: blueprintID, Name: tag, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	execSQL(t, s.db,
		`INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id) VALUES (?, 0, ?)`,
		blueprintID, promptID)
	return blueprintID
}

// capturedTaskClaims counts the task_claimed events the hub sends.
type capturedTaskClaims struct {
	mu sync.Mutex
	n  int
}

func (c *capturedTaskClaims) Publish(evt websocket.Event) {
	if evt.Type != "task_claimed" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *capturedTaskClaims) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// taskRow is every column of the task's row as the driver returns it, so two
// reads compare equal only if nothing in the row moved.
func taskRow(t *testing.T, database *sql.DB, taskID string) map[string]any {
	t.Helper()
	rows, err := database.Query(`SELECT * FROM tasks WHERE id = ?`, taskID)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("task columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("task %s not found", taskID)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		out[c] = vals[i]
	}
	return out
}

// blueprintUsage is the sum of every blueprint's usage_count.
func blueprintUsage(t *testing.T, database *sql.DB) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COALESCE(SUM(usage_count), 0) FROM blueprints`).Scan(&n); err != nil {
		t.Fatalf("read usage counts: %v", err)
	}
	return n
}

// blueprintRunCount is how many blueprint runs the task has.
func blueprintRunCount(t *testing.T, database *sql.DB, taskID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, taskID).Scan(&n); err != nil {
		t.Fatalf("count blueprint runs: %v", err)
	}
	return n
}

// reachOrgDailySpendCap sets the local org's daily spend cap and records
// today's spend above it.
func reachOrgDailySpendCap(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	set, err := s.allStores.Orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	set.MaxDailyCostUSD = 1
	if _, err := s.allStores.Orgs.UpdateSettingsVersioned(ctx, runmode.LocalDefaultOrgID, set, set.Version); err != nil {
		t.Fatalf("set the daily spend cap: %v", err)
	}
	now := time.Now().UTC()
	conversationID := uuid.NewString()
	execSQL(t, s.db, `
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, model, status, started_at)
		VALUES (?, ?, ?, ?, 'manual', 'manual', 'm', 'open', ?)`,
		conversationID, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now)
	execSQL(t, s.db, `
		INSERT INTO messages (org_id, conversation_id, role, subtype, content, cost_usd, created_at)
		VALUES (?, ?, 'assistant', '', 'work', 5, ?)`,
		runmode.LocalDefaultOrgID, conversationID, now)
}

// withTeamModels has the spawner resolve every team's models to models.
func withTeamModels(sp *delegate.Spawner, models domain.TeamModels) {
	sp.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return models, nil
	})
}

// A delegate the plan refuses writes nothing: the task a person holds, with
// the chat they opened on it, is exactly as it was, no run exists, no
// blueprint's usage moved, and nobody was told of a claim.
func TestTaskDelegate_RefusedByThePlanWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		// stage arranges the refusal and answers the blueprint to delegate
		// with.
		stage                 func(t *testing.T, s *Server, sp *delegate.Spawner) string
		status                int
		reason, field         string
		messageContains       string
		messageDoesNotContain string
	}{
		{
			name:   "a blueprint that doesn't exist",
			stage:  func(*testing.T, *Server, *delegate.Spawner) string { return "no-such-blueprint" },
			status: http.StatusUnprocessableEntity, reason: httpx.ReasonInvalidField, field: "blueprint_id",
		},
		{
			name: "a step whose model's provider is not connected",
			stage: func(t *testing.T, s *Server, _ *delegate.Spawner) string {
				connectOrgProviders(t, s, true, false)
				return seedOneStepBlueprint(t, s, "provider", modelKeyOn(t, modelcatalog.ProviderBedrock))
			},
			status: http.StatusConflict, reason: httpx.ReasonNotConfigured,
			messageContains: modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock),
		},
		{
			name: "a step whose model is not enabled",
			stage: func(t *testing.T, s *Server, sp *delegate.Spawner) string {
				withTeamModels(sp, domain.NewTeamModels(domain.ModelSonnet, domain.OrgModelSet([]string{domain.ModelSonnet}, nil)))
				return seedOneStepBlueprint(t, s, "disabled", domain.ModelOpus)
			},
			status: http.StatusConflict, reason: httpx.ReasonConflict,
			messageContains: domain.ModelOpus,
		},
		{
			name: "a team with no default model",
			stage: func(t *testing.T, s *Server, sp *delegate.Spawner) string {
				withTeamModels(sp, domain.NewTeamModels("", domain.ModelSet{}))
				return seedOneStepBlueprint(t, s, "nodefault", "")
			},
			status: http.StatusConflict, reason: httpx.ReasonNotConfigured,
			messageContains: "no default model",
		},
		{
			name: "a reached spend cap",
			stage: func(t *testing.T, s *Server, _ *delegate.Spawner) string {
				reachOrgDailySpendCap(t, s)
				return seedOneStepBlueprint(t, s, "capped", "")
			},
			status: http.StatusConflict, reason: httpx.ReasonConflict,
			messageContains: "daily spend cap is reached",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			sp := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku")
			s.SetSpawner(sp)

			taskID := seedTaskFixture(t, s.db, taskFixture{name: "refused-delegate", status: "queued"})
			claim := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, map[string]any{}))
			if claim.Code != http.StatusOK {
				t.Fatalf("claim = %d, want 200; body=%s", claim.Code, claim.Body.String())
			}
			var claimed struct {
				ConversationID string `json:"conversation_id"`
			}
			if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil || claimed.ConversationID == "" {
				t.Fatalf("the claim opened no chat: %s (%v)", claim.Body.String(), err)
			}
			blueprintID := c.stage(t, s, sp)

			before := taskRow(t, s.db, taskID)
			usage := blueprintUsage(t, s.db)
			claims := &capturedTaskClaims{}
			s.ws.SetBackplane(claims)

			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate",
				withTaskVersion(t, s, taskID, map[string]any{"blueprint_id": blueprintID}))
			if rec.Code != c.status {
				t.Fatalf("delegate = %d, want %d; body=%s", rec.Code, c.status, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, c.reason, c.field)
			if !strings.Contains(rec.Body.String(), c.messageContains) {
				t.Errorf("the refusal does not say %q: %s", c.messageContains, rec.Body.String())
			}

			if after := taskRow(t, s.db, taskID); !reflect.DeepEqual(after, before) {
				t.Errorf("the refused delegate changed the task:\nbefore %v\nafter  %v", before, after)
			}
			assertUntouchedPersonConversation(t, s.db, claimed.ConversationID)
			if n := blueprintRunCount(t, s.db, taskID); n != 0 {
				t.Errorf("blueprint runs on the task = %d, want 0", n)
			}
			if got := blueprintUsage(t, s.db); got != usage {
				t.Errorf("blueprint usage = %d, want %d: a refused delegate counted a run", got, usage)
			}
			if n := claims.count(); n != 0 {
				t.Errorf("task_claimed sent %d times, want 0", n)
			}
		})
	}
}

// A spend cap's refusal names the cap that was reached and who can raise it,
// and in multi mode nothing more: spend is an org admin's read, and the
// person delegating may not be one.
func TestWriteDelegatePlanRefusal_SpendCapNamesTheCapWithoutFigures(t *testing.T) {
	cases := []struct {
		err  *delegate.CostCapError
		want string
	}{
		{
			err:  &delegate.CostCapError{Scope: delegate.CostCapScopeOrg, SpentUSD: 12.5, CapUSD: 10},
			want: "The organization's daily spend cap is reached. An org admin can raise it in Settings.",
		},
		{
			err:  &delegate.CostCapError{Scope: delegate.CostCapScopeTeam, TeamName: "Platform", SpentUSD: 12.5, CapUSD: 10},
			want: "Team Platform's daily spend cap is reached. An org admin can raise it in Settings.",
		},
	}
	for _, c := range cases {
		t.Run(string(c.err.Scope), func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			rec := httptest.NewRecorder()
			writeDelegatePlanRefusal(rec, c.err)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409", rec.Code)
			}
			var body struct {
				Errors []httpx.ErrorItem `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if len(body.Errors) != 1 {
				t.Fatalf("errors = %+v, want one", body.Errors)
			}
			got := body.Errors[0]
			if got.Reason != httpx.ReasonConflict || got.Field != "" || got.Message != c.want {
				t.Errorf("error = %+v, want CONFLICT with no field and the message %q", got, c.want)
			}
		})
	}
}

// A fire that fails after the claim keeps the claim, and SPAWN_FAILED says so.
// Another run holding the task's one active run is what the fire meets when a
// second gesture or an event firing lands between the route's stop and its
// mint.
func TestTaskDelegate_FailureAfterTheClaimKeepsIt(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	blueprintID := seedOneStepBlueprint(t, s, "busy", "")
	taskID := seedTaskFixture(t, s.db, taskFixture{name: "busy-delegate", status: "queued"})
	seedBlueprintRunSQLite(t, s.db, taskID)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate",
		withTaskVersion(t, s, taskID, map[string]any{"blueprint_id": blueprintID}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delegate = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonSpawnFailed, "")

	stored, err := s.tasks.Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || stored == nil {
		t.Fatalf("read task back: task=%v err=%v", stored, err)
	}
	if stored.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("claimed_by_agent_id = %q, want the bot's claim to survive the failed fire", stored.ClaimedByAgentID)
	}
}

// A delegate that starts a run counts it against its blueprint once.
func TestTaskDelegate_CountsTheRunItStarts(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	blueprintID := seedOneStepBlueprint(t, s, "counted", "")
	taskID := seedTaskFixture(t, s.db, taskFixture{name: "counted-delegate", status: "queued"})

	delegateOnce(t, s, taskID, blueprintID)

	var usage int
	if err := s.db.QueryRow(`SELECT usage_count FROM blueprints WHERE id = ?`, blueprintID).Scan(&usage); err != nil {
		t.Fatalf("read usage count: %v", err)
	}
	if usage != 1 {
		t.Errorf("usage_count = %d, want 1", usage)
	}
}
