package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// taskGateRig is the viewer rig plus the five routes RequireTaskWrite gates,
// and two more callers: an org member on another team, who can see none of
// the rig team's tasks, and a viewer of the rig team who writes on a side
// team, which a task can name in task_teams.
type taskGateRig struct {
	*viewerRig
	mux      *http.ServeMux
	sideTeam string
	outsider string
	split    string
	entityID string
	eventID  string
}

func newTaskGateRig(t *testing.T) *taskGateRig {
	t.Helper()
	r := newViewerRig(t)

	otherTeam := pgtest.SeedTeam(t, r.h, r.orgID, "other-team")
	outsider := pgtest.SeedUser(t, r.h, "outsider")
	pgtest.AddOrgMember(t, r.h, outsider, r.orgID, otherTeam, "member", "member")

	sideTeam := pgtest.SeedTeam(t, r.h, r.orgID, "side-team")
	split := pgtest.SeedUser(t, r.h, "split")
	pgtest.AddOrgMember(t, r.h, split, r.orgID, r.teamID, "member", "viewer")
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, split, sideTeam)

	repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "repo")
	var entityID, eventID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
		VALUES ($1, 'github', 'octo/repo#1', 'pr', 'test pr', 'https://github.com', $2) RETURNING id
	`, r.orgID, repoID).Scan(&entityID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, r.orgID, entityID).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks/{id}", r.s.handleTaskGet)
	mux.HandleFunc("PATCH /api/tasks/{id}", r.s.handleTaskPatch)
	mux.HandleFunc("POST /api/tasks/{id}/claim", r.s.handleTaskClaim)
	mux.HandleFunc("POST /api/tasks/{id}/delegate", r.s.handleTaskDelegate)
	mux.HandleFunc("POST /api/tasks/{id}/requeue", r.s.handleRequeue)
	mux.HandleFunc("POST /api/tasks/{id}/undo", r.s.handleUndo)

	return &taskGateRig{
		viewerRig: r, mux: mux, sideTeam: sideTeam,
		outsider: outsider, split: split,
		entityID: entityID, eventID: eventID,
	}
}

// seedGateTask inserts a task on the rig team's entity, owned by the rig team.
// claimedBy, when set, is the user holding it, which puts it in progress.
// sharedWith, when set, adds a task_teams row for that team. dedup keeps the
// tasks apart under the one-active-task-per-entity-and-event index.
func (r *taskGateRig) seedGateTask(t *testing.T, dedup, visibility, creator, claimedBy, sharedWith string) string {
	t.Helper()
	status := "queued"
	var claimed any
	if claimedBy != "" {
		status, claimed = "in_progress", claimedBy
	}
	var id string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                   primary_event_id, status, claimed_by_user_id)
		VALUES ($1, $2, $3, $4, $5, 'github:pr:opened', $6, $7, $8, $9) RETURNING id
	`, r.orgID, creator, r.teamID, visibility, r.entityID, dedup, r.eventID, status, claimed).Scan(&id); err != nil {
		t.Fatalf("seed task %s: %v", dedup, err)
	}
	if sharedWith != "" {
		pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, id, sharedWith)
	}
	return id
}

func (r *taskGateRig) do(t *testing.T, caller, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: caller})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// taskRow is the whole stored task, so a refused request that wrote any
// column shows up as a difference.
func (r *taskGateRig) taskRow(t *testing.T, id string) string {
	t.Helper()
	var row string
	if err := r.h.AdminDB.QueryRow(`SELECT to_jsonb(t)::text FROM tasks t WHERE t.id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return row
}

func (r *taskGateRig) detailCanWrite(t *testing.T, caller, id string) any {
	t.Helper()
	rec := r.do(t, caller, http.MethodGet, "/api/tasks/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET task %s = %d, want 200; body=%s", id, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return body["can_write"]
}

var taskGateRoutes = []struct {
	name, method, suffix string
	body                 any
}{
	{"patch", http.MethodPatch, "", map[string]any{"status": "dismissed"}},
	{"claim", http.MethodPost, "/claim", map[string]any{}},
	{"delegate", http.MethodPost, "/delegate", map[string]any{}},
	{"requeue", http.MethodPost, "/requeue", nil},
	{"undo", http.MethodPost, "/undo", nil},
}

// TestTaskWriteGate_Postgres: the five task write routes answer a task the
// caller cannot see exactly as they answer an id that does not exist, so none
// of them says whether the task exists. A task the caller can see and may not
// write is 403, and a writer of one of a task's task_teams may write it only
// while it is unclaimed, as tasks_update says.
func TestTaskWriteGate_Postgres(t *testing.T) {
	r := newTaskGateRig(t)

	team := r.seedGateTask(t, "team", "team", r.admin, "", "")
	private := r.seedGateTask(t, "private", "private", r.member, "", "")
	claimedShared := r.seedGateTask(t, "claimed-shared", "team", r.admin, r.member, r.sideTeam)
	missing := uuid.NewString()

	// What each route answers for an id that does not exist: the body every
	// invisible task must match.
	missingBody := map[string]string{}
	for _, rt := range taskGateRoutes {
		rec := r.do(t, r.admin, rt.method, "/api/tasks/"+missing+rt.suffix, rt.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s on a missing task = %d, want 404; body=%s", rt.name, rec.Code, rec.Body.String())
		}
		missingBody[rt.name] = rec.Body.String()
	}

	cases := []struct {
		label, task, caller string
		want                int
	}{
		{"member of another team on a team task", team, r.outsider, http.StatusNotFound},
		{"team admin on someone else's private task", private, r.admin, http.StatusNotFound},
		{"team viewer on someone else's private task", private, r.viewer, http.StatusNotFound},
		{"team viewer on a team task", team, r.viewer, http.StatusForbidden},
		{"owning-team viewer, side-team writer, on a claimed task", claimedShared, r.split, http.StatusForbidden},
	}
	for _, tc := range cases {
		for _, rt := range taskGateRoutes {
			before := r.taskRow(t, tc.task)
			rec := r.do(t, tc.caller, rt.method, "/api/tasks/"+tc.task+rt.suffix, rt.body)
			switch tc.want {
			case http.StatusNotFound:
				if rec.Code != http.StatusNotFound || rec.Body.String() != missingBody[rt.name] {
					t.Errorf("%s, %s: got %d %s, want the missing-task answer 404 %s",
						tc.label, rt.name, rec.Code, rec.Body.String(), missingBody[rt.name])
				}
			case http.StatusForbidden:
				var env struct {
					Errors []httpx.ErrorItem `json:"errors"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &env)
				if rec.Code != http.StatusForbidden || len(env.Errors) != 1 || env.Errors[0].Reason != httpx.ReasonForbidden {
					t.Errorf("%s, %s: got %d %s, want one FORBIDDEN error", tc.label, rt.name, rec.Code, rec.Body.String())
				}
			}
			if after := r.taskRow(t, tc.task); after != before {
				t.Errorf("%s, %s: refused request changed the task\nbefore %s\nafter  %s", tc.label, rt.name, before, after)
			}
		}
	}

	// The detail read agrees with the routes: the split caller sees the claimed
	// task and may not write it.
	if got := r.detailCanWrite(t, r.split, claimedShared); got != false {
		t.Errorf("split caller GET claimed task: can_write = %v, want false", got)
	}

	// Unclaimed, the same task is the side team's to write: the detail read
	// says so, and a write passes the gate and lands under RLS.
	unclaimedShared := r.seedGateTask(t, "unclaimed-shared", "team", r.admin, "", r.sideTeam)
	if got := r.detailCanWrite(t, r.split, unclaimedShared); got != true {
		t.Errorf("split caller GET unclaimed task: can_write = %v, want true", got)
	}
	rec := r.do(t, r.split, http.MethodPatch, "/api/tasks/"+unclaimedShared, map[string]any{"status": "dismissed"})
	if rec.Code != http.StatusOK {
		t.Errorf("split caller dismiss of the unclaimed task = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
