package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// claimTeamRig is a Postgres-backed Server, with RLS live, whose one person
// writes two teams, each with the bot enabled and a blueprint of its own,
// beside a third team the person is not on. first is the team with the lower
// id, which a claim that named no team used to land on.
type claimTeamRig struct {
	h               *pgtest.Harness
	s               *Server
	mux             *http.ServeMux
	orgID, user     string
	agentID         string
	first, second   string
	outside         string
	firstBlueprint  string
	secondBlueprint string
}

func newClaimTeamRig(t *testing.T) *claimTeamRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	r := &claimTeamRig{h: h, s: New(h.AdminDB, stores)}
	r.orgID, r.user, r.first = pgtest.SeedOrgWithUser(t, h, "claimant")
	r.second = pgtest.SeedTeam(t, h, r.orgID, "team-2")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, r.user, r.second)
	if r.second < r.first {
		r.first, r.second = r.second, r.first
	}
	r.outside = pgtest.SeedTeam(t, h, r.orgID, "outside")
	if err := h.AdminDB.QueryRow(
		`INSERT INTO agents (org_id, display_name) VALUES ($1, 'Test Bot') RETURNING id::text`, r.orgID,
	).Scan(&r.agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for _, team := range []string{r.first, r.second, r.outside} {
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO team_agents (team_id, agent_id, enabled) VALUES ($1, $2, TRUE)`, team, r.agentID)
	}
	r.firstBlueprint = r.seedBlueprint(t, r.first, "first-team")
	r.secondBlueprint = r.seedBlueprint(t, r.second, "second-team")

	// The plan's model checks are not what these tests are about: the org has
	// a provider connected, and every team gets one default model on it.
	if _, err := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).Orgs.SetAnthropicKeyRef(
		t.Context(), r.orgID, secretKeyAnthropicAPIKey); err != nil {
		t.Fatalf("connect anthropic: %v", err)
	}
	sp := delegate.NewSpawner(h.AdminDB, stores, nil, r.s.ws, "claude-sonnet-4-6")
	withTeamModels(sp, domain.NewTeamModels(domain.ModelSonnet, domain.ModelSet{}))
	r.s.SetSpawner(sp)

	r.mux = http.NewServeMux()
	r.mux.HandleFunc("GET /api/tasks/{id}", r.s.handleTaskGet)
	r.mux.HandleFunc("POST /api/tasks/{id}/claim", r.s.handleTaskClaim)
	r.mux.HandleFunc("POST /api/tasks/{id}/delegate", r.s.handleTaskDelegate)
	return r
}

// seedBlueprint inserts a one-step blueprint owned by teamID and returns its
// id.
func (r *claimTeamRig) seedBlueprint(t *testing.T, teamID, name string) string {
	t.Helper()
	promptID, blueprintID := uuid.NewString(), uuid.NewString()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools)
		VALUES ($1, $2, $3, $4, $5, 'do the thing', 'user', '[]'::jsonb)`,
		promptID, r.orgID, teamID, r.user, name)
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, source, name)
		VALUES ($1, $2, $3, $4, 'user', $5)`,
		blueprintID, r.orgID, teamID, r.user, name)
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id)
		VALUES ($1, $2, $3, 0, $4)`,
		r.orgID, teamID, blueprintID, promptID)
	return blueprintID
}

// seedTask inserts a queued, unclaimed task owned by owner ("" for no owner)
// that both of the person's teams can see.
func (r *claimTeamRig) seedTask(t *testing.T, owner string) string {
	t.Helper()
	tag := uuid.NewString()[:8]
	repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "claim-team")
	var entityID, eventID, taskID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
		VALUES ($1, 'github', $2, 'pr', 'test pr', 'https://github.com', $3) RETURNING id
	`, r.orgID, "octo/claim-team#"+tag, repoID).Scan(&entityID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, r.orgID, entityID).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, status)
		VALUES ($1, $2, NULLIF($3, '')::uuid, $4, 'github:pr:opened', $5, 'queued') RETURNING id
	`, r.orgID, r.user, owner, entityID, eventID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2), ($1, $3)`, taskID, r.first, r.second)
	return taskID
}

func (r *claimTeamRig) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return r.doAs(t, r.user, method, path, body)
}

func (r *claimTeamRig) doAs(t *testing.T, caller, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	vr := &viewerRig{orgID: r.orgID}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, vr.req(method, path, caller, body))
	return rec
}

func (r *claimTeamRig) post(t *testing.T, taskID, verb string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return r.do(t, http.MethodPost, "/api/tasks/"+taskID+"/"+verb, withTaskVersionIn(t, r.s, r.orgID, taskID, body))
}

// taskRow is the whole stored task, so a refused request that wrote any
// column shows up as a difference.
func (r *claimTeamRig) taskRow(t *testing.T, taskID string) string {
	t.Helper()
	var row string
	if err := r.h.AdminDB.QueryRow(`SELECT to_jsonb(t)::text FROM tasks t WHERE t.id = $1`, taskID).Scan(&row); err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return row
}

// landed reads what a claim writes: the task's team and who holds it.
func (r *claimTeamRig) landed(t *testing.T, taskID string) (team, user, agent string) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(`
		SELECT COALESCE(team_id::text, ''), COALESCE(claimed_by_user_id::text, ''), COALESCE(claimed_by_agent_id::text, '')
		  FROM tasks WHERE id = $1`, taskID).Scan(&team, &user, &agent); err != nil {
		t.Fatalf("read task: %v", err)
	}
	return team, user, agent
}

// conversationTeams is the teams of the task's conversations.
func (r *claimTeamRig) conversationTeams(t *testing.T, taskID string) []string {
	t.Helper()
	rows, err := r.h.AdminDB.Query(`SELECT team_id::text FROM conversations WHERE task_id = $1 ORDER BY started_at, id`, taskID)
	if err != nil {
		t.Fatalf("read conversations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var team string
		if err := rows.Scan(&team); err != nil {
			t.Fatalf("scan conversation: %v", err)
		}
		out = append(out, team)
	}
	return out
}

func (r *claimTeamRig) blueprintRuns(t *testing.T, taskID string) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = $1`, taskID).Scan(&n); err != nil {
		t.Fatalf("count blueprint runs: %v", err)
	}
	return n
}

// refusal is what a refused claim or delegate answers.
type refusal struct {
	status        int
	reason, field string
}

func (r *claimTeamRig) assertRefusedAndUnwritten(t *testing.T, taskID, verb string, body map[string]any, want refusal) {
	t.Helper()
	before := r.taskRow(t, taskID)
	rec := r.post(t, taskID, verb, body)
	if rec.Code != want.status {
		t.Fatalf("%s = %d, want %d; body=%s", verb, rec.Code, want.status, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, want.reason, want.field)
	if after := r.taskRow(t, taskID); after != before {
		t.Errorf("the refused %s changed the task:\nbefore %s\nafter  %s", verb, before, after)
	}
	if teams := r.conversationTeams(t, taskID); len(teams) != 0 {
		t.Errorf("the refused %s left conversations on teams %v", verb, teams)
	}
}

var (
	teamRequired     = refusal{http.StatusBadRequest, httpx.ReasonInvalidField, "team_id"}
	teamNotCandidate = refusal{http.StatusUnprocessableEntity, httpx.ReasonCrossTeamRef, "team_id"}
	blueprintOtherTm = refusal{http.StatusUnprocessableEntity, httpx.ReasonCrossTeamRef, "blueprint_id"}
)

// The delegate route resolves the team and the blueprint together: for a task
// two of the caller's teams can take, it lands on the team the body names and
// runs only that team's blueprint, and every refusal leaves the task as it was.
func TestTaskDelegate_NamesTheTeamAndRunsItsBlueprint(t *testing.T) {
	r := newClaimTeamRig(t)

	refused := []struct {
		name string
		body map[string]any
		want refusal
	}{
		{"no team_id", map[string]any{"blueprint_id": r.secondBlueprint}, teamRequired},
		{"a team_id that is not a team id", map[string]any{"team_id": "team-2", "blueprint_id": r.secondBlueprint}, teamRequired},
		{"the second team with the first team's blueprint",
			map[string]any{"team_id": r.second, "blueprint_id": r.firstBlueprint}, blueprintOtherTm},
		{"the first team with the second team's blueprint",
			map[string]any{"team_id": r.first, "blueprint_id": r.secondBlueprint}, blueprintOtherTm},
		{"a team that is not a candidate",
			map[string]any{"team_id": r.outside, "blueprint_id": r.secondBlueprint}, teamNotCandidate},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			taskID := r.seedTask(t, "")
			r.assertRefusedAndUnwritten(t, taskID, "delegate", tc.body, tc.want)
			if n := r.blueprintRuns(t, taskID); n != 0 {
				t.Errorf("blueprint runs on the task = %d, want 0", n)
			}
		})
	}

	t.Run("the second team with its own blueprint", func(t *testing.T) {
		taskID := r.seedTask(t, "")
		rec := r.post(t, taskID, "delegate", map[string]any{"team_id": r.second, "blueprint_id": r.secondBlueprint})
		if rec.Code != http.StatusOK {
			t.Fatalf("delegate = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if team, _, agent := r.landed(t, taskID); team != r.second || agent != r.agentID {
			t.Errorf("task = (team %s, agent %q), want the bot's claim on the second team %s", team, agent, r.second)
		}
		if teams := r.conversationTeams(t, taskID); !slices.Equal(teams, []string{r.second}) {
			t.Errorf("the run's conversations are on teams %v, want [%s]", teams, r.second)
		}
	})
}

// The claim route resolves the team the same way.
func TestTaskClaim_NamesTheTeam(t *testing.T) {
	r := newClaimTeamRig(t)

	t.Run("no team_id", func(t *testing.T) {
		taskID := r.seedTask(t, "")
		r.assertRefusedAndUnwritten(t, taskID, "claim", map[string]any{}, teamRequired)
	})
	t.Run("a team that is not a candidate", func(t *testing.T) {
		taskID := r.seedTask(t, "")
		r.assertRefusedAndUnwritten(t, taskID, "claim", map[string]any{"team_id": r.outside}, teamNotCandidate)
	})
	t.Run("the second team", func(t *testing.T) {
		taskID := r.seedTask(t, "")
		rec := r.post(t, taskID, "claim", map[string]any{"team_id": r.second})
		if rec.Code != http.StatusOK {
			t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if team, user, _ := r.landed(t, taskID); team != r.second || user != r.user {
			t.Errorf("task = (team %s, user %q), want the caller's claim on the second team %s", team, user, r.second)
		}
		if teams := r.conversationTeams(t, taskID); !slices.Equal(teams, []string{r.second}) {
			t.Errorf("the claim's conversation is on teams %v, want [%s]", teams, r.second)
		}
	})
	t.Run("a team_id beside target_user_id", func(t *testing.T) {
		taskID := r.seedTask(t, "")
		r.assertRefusedAndUnwritten(t, taskID, "claim",
			map[string]any{"team_id": r.second, "target_user_id": uuid.NewString()}, teamRequired)
	})
}

// A task that already belongs to a team the caller writes lands there with no
// team_id, on both routes, and refuses another team though the caller writes
// it and it can see the task.
func TestTaskClaimAndDelegate_AnOwnedTaskLandsOnItsTeam(t *testing.T) {
	r := newClaimTeamRig(t)

	t.Run("claim", func(t *testing.T) {
		taskID := r.seedTask(t, r.second)
		r.assertRefusedAndUnwritten(t, taskID, "claim", map[string]any{"team_id": r.first}, teamNotCandidate)

		rec := r.post(t, taskID, "claim", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if team, user, _ := r.landed(t, taskID); team != r.second || user != r.user {
			t.Errorf("task = (team %s, user %q), want the caller's claim on the owning team %s", team, user, r.second)
		}
	})
	t.Run("delegate", func(t *testing.T) {
		taskID := r.seedTask(t, r.second)
		r.assertRefusedAndUnwritten(t, taskID, "delegate",
			map[string]any{"team_id": r.first, "blueprint_id": r.firstBlueprint}, teamNotCandidate)

		rec := r.post(t, taskID, "delegate", map[string]any{"blueprint_id": r.secondBlueprint})
		if rec.Code != http.StatusOK {
			t.Fatalf("delegate = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if team, _, agent := r.landed(t, taskID); team != r.second || agent != r.agentID {
			t.Errorf("task = (team %s, agent %q), want the bot's claim on the owning team %s", team, agent, r.second)
		}
	})
}

// GET /api/tasks/{id} lists the teams the caller's claim may land the task
// on: what ClaimTeamCandidates answers for them, in its order.
func TestTaskGet_ListsTheClaimTeams(t *testing.T) {
	r := newClaimTeamRig(t)

	for _, tc := range []struct {
		name, owner string
		want        []string
	}{
		{"an unowned task both teams see", "", []string{r.first, r.second}},
		{"a task the second team owns", r.second, []string{r.second}},
		{"a task a team the caller is not on owns", r.outside, []string{r.first, r.second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := r.seedTask(t, tc.owner)
			var stored []string
			if err := r.h.WithUser(t, r.user, r.orgID, func(tx *sql.Tx) error {
				var e error
				stored, e = pgstore.NewForTx(tx, pgtest.SecretKey).Tasks.ClaimTeamCandidates(t.Context(), r.orgID, taskID, r.user)
				return e
			}); err != nil {
				t.Fatalf("ClaimTeamCandidates: %v", err)
			}
			if !slices.Equal(stored, tc.want) {
				t.Fatalf("ClaimTeamCandidates = %v, want %v", stored, tc.want)
			}

			rec := r.do(t, http.MethodGet, "/api/tasks/"+taskID, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET task = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				ClaimTeamIDs *[]string `json:"claim_team_ids"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode task: %v", err)
			}
			if body.ClaimTeamIDs == nil || !slices.Equal(*body.ClaimTeamIDs, stored) {
				t.Errorf("claim_team_ids = %v, want %v", body.ClaimTeamIDs, stored)
			}
		})
	}

	// No candidate is an empty list, not an absent key: absent means the read
	// failed. A viewer of a team that sees the task reads it and can land it
	// nowhere.
	t.Run("a task the caller can see and no team of theirs can take", func(t *testing.T) {
		viewer := pgtest.SeedUser(t, r.h, "viewer")
		pgtest.AddOrgMember(t, r.h, viewer, r.orgID, r.first, "member", "viewer")
		taskID := r.seedTask(t, "")
		rec := r.doAs(t, viewer, http.MethodGet, "/api/tasks/"+taskID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET task = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode task: %v", err)
		}
		if string(body["claim_team_ids"]) != "[]" {
			t.Errorf("claim_team_ids = %q, want []", body["claim_team_ids"])
		}
	})
}
