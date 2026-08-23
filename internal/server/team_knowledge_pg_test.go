package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The half of the team-KB contract N=1 cannot express: two teams, real
// memberships, and the visibility boundary between them. `shared/` is
// org-readable through the owning team's own routes; `private/` is readable
// only through that team's gate and is invisible — not forbidden — to everyone
// else; and nothing ever writes another team's KB.

type kbRig struct {
	h     *pgtest.Harness
	s     *Server
	orgID string
	// alpha and beta are two teams in one org. alphaMember is on alpha only,
	// betaMember on beta only, and viewer is on alpha with a view-only role.
	alpha, beta             string
	alphaMember, betaMember string
	viewer                  string
	orgAdmin                string
}

func newKBRig(t *testing.T) *kbRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	s.SetTeamKB(kbstore.NewLocalAt(t.TempDir()))

	orgID, founder, alpha := pgtest.SeedOrgWithUser(t, h, "founder")
	beta := pgtest.SeedTeam(t, h, orgID, "beta")

	betaMember := pgtest.SeedUser(t, h, "beta-member")
	pgtest.AddOrgMember(t, h, betaMember, orgID, beta, "member", "admin")
	viewer := pgtest.SeedUser(t, h, "alpha-viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, alpha, "member", "viewer")
	orgAdmin := pgtest.SeedUser(t, h, "org-admin")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`, orgAdmin, orgID)

	return &kbRig{
		h: h, s: s, orgID: orgID,
		alpha: alpha, beta: beta,
		alphaMember: founder, betaMember: betaMember,
		viewer: viewer, orgAdmin: orgAdmin,
	}
}

// as builds a request carrying callerID's claims and the rig's active org —
// the state withSession would supply — with the {team_id} path value set.
func (r *kbRig) as(callerID, teamID, method, suffix string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/api/teams/"+teamID+"/kb"+suffix, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("team_id", teamID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

func (r *kbRig) list(t *testing.T, callerID, teamID string, body map[string]any) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.s.handleTeamKnowledgeList(rec, r.as(callerID, teamID, http.MethodPost, "/list", body))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out struct {
		Items []knowledgeFileJSON `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return rec, refsOf(out.Items)
}

// upload writes files into teamID's KB as callerID.
func (r *kbRig) upload(t *testing.T, callerID, teamID, root string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("root", root); err != nil {
		t.Fatalf("write root: %v", err)
	}
	for name, content := range files {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := r.as(callerID, teamID, http.MethodPost, "/files", nil)
	req.Body = http.NoBody
	req = httptest.NewRequest(http.MethodPost, "/api/teams/"+teamID+"/kb/files", &body).WithContext(req.Context())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("team_id", teamID)
	rec := httptest.NewRecorder()
	r.s.handleTeamKnowledgeUpload(rec, req)
	return rec
}

func (r *kbRig) seedAlphaKB(t *testing.T) {
	t.Helper()
	if rec := r.upload(t, r.alphaMember, r.alpha, "private", map[string]string{"secret.md": "ours alone"}); rec.Code != http.StatusOK {
		t.Fatalf("seed private: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.upload(t, r.alphaMember, r.alpha, "shared", map[string]string{"published.md": "for the org"}); rec.Code != http.StatusOK {
		t.Fatalf("seed shared: %d %s", rec.Code, rec.Body.String())
	}
}

// TestTeamKnowledge_RootsAreTheVisibilityModel is the core boundary: the same
// route, on the same team, answers a different set of roots depending on
// whether the caller is on that team.
func TestTeamKnowledge_RootsAreTheVisibilityModel(t *testing.T) {
	r := newKBRig(t)
	r.seedAlphaKB(t)

	_, mine := r.list(t, r.alphaMember, r.alpha, map[string]any{})
	if !equalSlices(mine, []string{"private/secret.md", "shared/published.md"}) {
		t.Fatalf("member list = %v; want both roots", mine)
	}

	// A member of another team in the same org reads alpha's published root
	// through alpha's own route — and nothing else.
	_, theirs := r.list(t, r.betaMember, r.alpha, map[string]any{})
	if !equalSlices(theirs, []string{"shared/published.md"}) {
		t.Fatalf("non-member list = %v; want the shared root alone", theirs)
	}

	// An explicit private filter from a non-member is not an error: it is a
	// filter over knowledge they cannot see, and the empty page it answers is
	// the same one a team with no private notes answers.
	rec, filtered := r.list(t, r.betaMember, r.alpha, map[string]any{"root": "private"})
	if rec.Code != http.StatusOK || len(filtered) != 0 {
		t.Fatalf("non-member private filter = %d %v; want 200 and an empty page", rec.Code, filtered)
	}

	// An org admin is not a member of alpha, and org-admin is not a way onto a
	// team's private knowledge.
	_, asAdmin := r.list(t, r.orgAdmin, r.alpha, map[string]any{})
	if !equalSlices(asAdmin, []string{"shared/published.md"}) {
		t.Fatalf("org-admin list = %v; want the shared root alone", asAdmin)
	}
}

// TestTeamKnowledge_PrivateReadIs404ToANonMember: invisible, not forbidden. A
// 403 would confirm the document exists.
func TestTeamKnowledge_PrivateReadIs404ToANonMember(t *testing.T) {
	r := newKBRig(t)
	r.seedAlphaKB(t)

	read := func(caller, path string) *httptest.ResponseRecorder {
		req := r.as(caller, r.alpha, http.MethodGet, "/"+path, nil)
		req.SetPathValue("path", path)
		rec := httptest.NewRecorder()
		r.s.handleTeamKnowledgeRead(rec, req)
		return rec
	}

	if rec := read(r.alphaMember, "private/secret.md"); rec.Code != http.StatusOK {
		t.Fatalf("member read = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := read(r.betaMember, "private/secret.md"); rec.Code != http.StatusNotFound {
		t.Fatalf("non-member private read = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// The same caller reads the published document fine, which is what makes
	// the 404 above a statement about the root rather than about the team.
	if rec := read(r.betaMember, "shared/published.md"); rec.Code != http.StatusOK {
		t.Fatalf("non-member shared read = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTeamKnowledge_WritesNeverCrossTeams: a member of one team cannot write
// another's KB, in either root, through any of the three write verbs.
func TestTeamKnowledge_WritesNeverCrossTeams(t *testing.T) {
	r := newKBRig(t)
	r.seedAlphaKB(t)

	for _, root := range []string{"private", "shared"} {
		if rec := r.upload(t, r.betaMember, r.alpha, root, map[string]string{"intrusion.md": "x"}); rec.Code != http.StatusForbidden {
			t.Errorf("cross-team upload into %s = %d, want 403; body=%s", root, rec.Code, rec.Body.String())
		}
	}

	del := httptest.NewRecorder()
	delReq := r.as(r.betaMember, r.alpha, http.MethodDelete, "/shared/published.md", nil)
	delReq.SetPathValue("path", "shared/published.md")
	r.s.handleTeamKnowledgeDelete(del, delReq)
	if del.Code != http.StatusForbidden {
		t.Errorf("cross-team delete = %d, want 403; body=%s", del.Code, del.Body.String())
	}

	mv := httptest.NewRecorder()
	r.s.handleTeamKnowledgeMove(mv, r.as(r.betaMember, r.alpha, http.MethodPost, "/move", map[string]any{
		"from_root": "shared", "from_path": "published.md",
		"to_root": "private", "to_path": "published.md",
	}))
	if mv.Code != http.StatusForbidden {
		t.Errorf("cross-team move = %d, want 403; body=%s", mv.Code, mv.Body.String())
	}

	// Nothing changed: alpha's KB is exactly as it was.
	_, still := r.list(t, r.alphaMember, r.alpha, map[string]any{})
	if !equalSlices(still, []string{"private/secret.md", "shared/published.md"}) {
		t.Fatalf("alpha's KB after the refused writes = %v", still)
	}
}

// TestTeamKnowledge_ViewerReadsButDoesNotWrite: the view-only role is the same
// boundary here as at every other team-scoped write.
func TestTeamKnowledge_ViewerReadsButDoesNotWrite(t *testing.T) {
	r := newKBRig(t)
	r.seedAlphaKB(t)

	_, seen := r.list(t, r.viewer, r.alpha, map[string]any{})
	if !equalSlices(seen, []string{"private/secret.md", "shared/published.md"}) {
		t.Fatalf("viewer list = %v; a viewer is a member and reads both roots", seen)
	}

	rec := r.upload(t, r.viewer, r.alpha, "private", map[string]string{"note.md": "x"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer upload = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonForbidden, "")
}

// TestTeamKnowledge_PublishMakesItOrgReadable is the round trip the two-root
// model exists for: a document nobody outside the team could see becomes one
// every org member can, by moving.
func TestTeamKnowledge_PublishMakesItOrgReadable(t *testing.T) {
	r := newKBRig(t)
	if rec := r.upload(t, r.alphaMember, r.alpha, "private", map[string]string{"runbook.md": "steps"}); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	_, before := r.list(t, r.betaMember, r.alpha, map[string]any{})
	if len(before) != 0 {
		t.Fatalf("non-member saw %v before publishing", before)
	}

	mv := httptest.NewRecorder()
	r.s.handleTeamKnowledgeMove(mv, r.as(r.alphaMember, r.alpha, http.MethodPost, "/move", map[string]any{
		"from_root": "private", "from_path": "runbook.md",
		"to_root": "shared", "to_path": "runbook.md",
	}))
	if mv.Code != http.StatusOK {
		t.Fatalf("publish = %d, want 200; body=%s", mv.Code, mv.Body.String())
	}

	_, after := r.list(t, r.betaMember, r.alpha, map[string]any{})
	if !equalSlices(after, []string{"shared/runbook.md"}) {
		t.Fatalf("non-member list after publish = %v; want the published document", after)
	}
}

// TestTeamKnowledge_TeamsDoNotShareAKB: two teams' knowledge bases are
// separate stores addressed by separate routes, not one namespace with a
// filter over it.
func TestTeamKnowledge_TeamsDoNotShareAKB(t *testing.T) {
	r := newKBRig(t)
	if rec := r.upload(t, r.alphaMember, r.alpha, "shared", map[string]string{"mine.md": "alpha"}); rec.Code != http.StatusOK {
		t.Fatalf("alpha seed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.upload(t, r.betaMember, r.beta, "shared", map[string]string{"mine.md": "beta"}); rec.Code != http.StatusOK {
		t.Fatalf("beta seed: %d %s", rec.Code, rec.Body.String())
	}

	req := r.as(r.betaMember, r.beta, http.MethodGet, "/shared/mine.md", nil)
	req.SetPathValue("path", "shared/mine.md")
	rec := httptest.NewRecorder()
	r.s.handleTeamKnowledgeRead(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "beta" {
		t.Fatalf("beta read = %d %q; want beta's own document", rec.Code, rec.Body.String())
	}
}

// TestTeamKnowledge_CrossOrgTeamIs404 pins the outer boundary: a real team id
// from another tenant names nothing here.
func TestTeamKnowledge_CrossOrgTeamIs404(t *testing.T) {
	r := newKBRig(t)
	_, otherFounder, otherTeam := pgtest.SeedOrgWithUser(t, r.h, "elsewhere")
	_ = otherFounder

	rec, _ := r.list(t, r.alphaMember, otherTeam, map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org list = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
