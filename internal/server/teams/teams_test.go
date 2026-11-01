package teams

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// localRig is the teams handler over a local-mode store with the schema the
// binary boots with, which seeds the local org, team and user.
type localRig struct {
	db *sql.DB
	th *Handler
}

func newLocalRig(t *testing.T) *localRig {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)
	return &localRig{db: conn, th: New(stores.Tx, authz.New(conn, stores.Tx), stores, nil, nil)}
}

// seedTeam inserts an extra team into the local org. SQLite's ListForUser
// returns every team in the org, so one more row makes the local caller a
// member of two teams.
func (l *localRig) seedTeam(t *testing.T, slug string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := l.db.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		id, runmode.LocalDefaultOrgID, slug, slug,
	); err != nil {
		t.Fatalf("seed team %s: %v", slug, err)
	}
	return id
}

// localRequest is a request as the local user in the local org — the identity
// the session middleware seeds in local mode — carrying body as JSON.
func localRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithOrgID(r.Context(), runmode.LocalDefaultOrgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: runmode.LocalDefaultUserID})
	return r.WithContext(ctx)
}

// list is POST /api/teams/list with body.
func (l *localRig) list(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	l.th.handleTeamsList(rec, localRequest(t, http.MethodPost, "/api/teams/list", body))
	return rec
}

// get is GET /api/teams/{team_id} with addr as the path segment.
func (l *localRig) get(t *testing.T, addr string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(t, http.MethodGet, "/api/teams/"+addr, nil)
	req.SetPathValue("team_id", addr)
	rec := httptest.NewRecorder()
	l.th.handleTeamGet(rec, req)
	return rec
}

// TestTeamsList_LocalReturnsSoleTeam: in local mode (N=1) the teams list
// returns the single local team and marks no sticky default — the frontend's
// ≥2 gate keeps every selector hidden off this one-team response.
func TestTeamsList_LocalReturnsSoleTeam(t *testing.T) {
	l := newLocalRig(t)

	page := httpxtest.DecodeList[teamJSON](t, l.list(t, map[string]any{}))
	if len(page.Items) != 1 || page.Total() != 1 {
		t.Fatalf("teams = %d (total %d), want 1 / 1", len(page.Items), page.Total())
	}
	if page.Items[0].ID != runmode.LocalDefaultTeamID {
		t.Errorf("team id = %q, want %q", page.Items[0].ID, runmode.LocalDefaultTeamID)
	}
	// Local N=1: the sole user admins the sole team, so the settings
	// surface's Team-section gate (role == "admin") reads true.
	if page.Items[0].Role != "admin" {
		t.Errorf("role = %q, want %q (local sole team)", page.Items[0].Role, "admin")
	}
	if page.Items[0].IsLastActing {
		t.Errorf("is_last_acting = true, want false (no sticky default set)")
	}
}

// TestTeamsList_TwoTeams: with a second team seeded the list returns both, and
// one-row pages partition the set rather than repeating its head.
func TestTeamsList_TwoTeams(t *testing.T) {
	l := newLocalRig(t)
	second := l.seedTeam(t, "second")

	page := httpxtest.DecodeList[teamJSON](t, l.list(t, map[string]any{}))
	if len(page.Items) != 2 || page.Total() != 2 {
		t.Fatalf("teams = %d (total %d), want 2 / 2", len(page.Items), page.Total())
	}
	got := map[string]bool{}
	for _, tm := range page.Items {
		got[tm.ID] = true
	}
	if !got[runmode.LocalDefaultTeamID] || !got[second] {
		t.Errorf("teams = %+v; want both default and %s", page.Items, second)
	}

	first := httpxtest.DecodeList[teamJSON](t, l.list(t, map[string]any{"page_size": 1}))
	if len(first.Items) != 1 || first.Total() != 2 || first.NextPageToken == "" {
		t.Fatalf("page 1 = %+v (total %d, token %q), want one row, total 2, a next token",
			first.Items, first.Total(), first.NextPageToken)
	}
	next := httpxtest.DecodeList[teamJSON](t, l.list(t,
		map[string]any{"page_size": 1, "page_token": first.NextPageToken}))
	if len(next.Items) != 1 || next.Items[0].ID == first.Items[0].ID {
		t.Errorf("page 2 = %+v, want the other team (pages must partition the set)", next.Items)
	}
}

// The single read is the only reader of a team's description, and it answers
// with the list's row shape. {team_id} takes the local "default" alias.
func TestTeamGet_Local(t *testing.T) {
	l := newLocalRig(t)

	for _, addr := range []string{runmode.LocalDefaultTeamID, "default"} {
		rec := l.get(t, addr)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/teams/%s = %d, want 200; body=%s", addr, rec.Code, rec.Body.String())
		}
		var got teamJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != runmode.LocalDefaultTeamID || got.Role != "admin" {
			t.Errorf("GET /api/teams/%s = %+v, want the local team as admin", addr, got)
		}
		if got.ArchivedAt != "" {
			t.Errorf("archived_at = %q, want empty for a live team", got.ArchivedAt)
		}
	}

	// A well-formed id for a team that doesn't exist is not-found, and so is a
	// segment that is not an id at all — a path id that names nothing must not
	// reach the store and must not disclose which of the two it was.
	for _, id := range []string{"11111111-1111-1111-1111-111111111111", "not-an-id"} {
		if rec := l.get(t, id); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/teams/%s = %d, want 404; body=%s", id, rec.Code, rec.Body.String())
		}
	}
}

// TestTeamCreate_LocalIsNotFound: "add team" is multi-only; in local
// mode POST /api/teams is 404 (the feature is absent at N=1).
func TestTeamCreate_LocalIsNotFound(t *testing.T) {
	l := newLocalRig(t)

	rec := httptest.NewRecorder()
	l.th.handleTeamCreate(rec, localRequest(t, http.MethodPost, "/api/teams", map[string]string{"name": "Platform"}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (multi-only); body=%s", rec.Code, rec.Body.String())
	}
}
