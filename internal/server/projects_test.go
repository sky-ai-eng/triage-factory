package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// doMultipartUpload posts files (one per map entry, all under the
// "file" form key) to the given path and returns the recorded
// response. Mirrors doJSON's call shape so the test bodies stay flat.
func doMultipartUpload(t *testing.T, s *Server, path string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, body := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatalf("create form file %q: %v", name, err)
		}
		if _, err := fw.Write(body); err != nil {
			t.Fatalf("write form file %q: %v", name, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doBundleImport(t *testing.T, s *Server, bundle []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("bundle", "project.tfproject")
	if err != nil {
		t.Fatalf("create bundle form file: %v", err)
	}
	if _, err := fw.Write(bundle); err != nil {
		t.Fatalf("write bundle form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/projects/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestProjectCreate_Happy(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":         "Triage Factory",
		"description":  "Local-first triage UI",
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Error("expected generated id")
	}
	if got.Name != "Triage Factory" {
		t.Errorf("name = %q", got.Name)
	}
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "sky-ai-eng/triage-factory" {
		t.Errorf("pinned_repos = %v", got.PinnedRepos)
	}
}

// TestProjectCreate_BindsToOrgDefaultTeam pins the SKY-340 fix:
// handleProjectCreate must resolve team_id via TeamsStore.GetDefaultForOrgSystem
// for the request's authenticated org rather than hardcoding the
// local-mode sentinel. In local mode that resolves to the same
// sentinel team, but the *path* matters — in multi mode the same
// code branch would resolve to the requesting tenant's default team,
// so the SQL inserts a FK-valid row.
func TestProjectCreate_BindsToOrgDefaultTeam(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name": "Bound Project",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// team_id isn't projected through the Project struct; read it
	// directly so the test asserts on what the SQL actually wrote
	// rather than what the JSON response chose to surface.
	var teamID string
	if err := s.db.QueryRow(`SELECT team_id FROM projects WHERE id = ?`, got.ID).Scan(&teamID); err != nil {
		t.Fatalf("read team_id: %v", err)
	}
	wantTeamID, err := s.teams.GetDefaultForOrgSystem(t.Context(), runmode.LocalDefaultOrg)
	if err != nil {
		t.Fatalf("resolve expected team: %v", err)
	}
	if teamID != wantTeamID {
		t.Errorf("team_id = %q; want %q (default team for org %s)", teamID, wantTeamID, runmode.LocalDefaultOrg)
	}
}

func TestProjectCreate_RejectsEmptyName(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"", "   ", "\t"} {
		rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{"name": name})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q status = %d, want 400", name, rec.Code)
		}
	}
}

func TestProjectCreate_RejectsBadPinnedRepoSlugs(t *testing.T) {
	s := newTestServer(t)
	bad := [][]string{
		{""},
		{"  "},
		{"justaword"},
		{"too/many/slashes"},
		{"/missing-owner"},
		{"missing-repo/"},
	}
	for _, repos := range bad {
		rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
			"name":         "P",
			"pinned_repos": repos,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repos=%v status = %d, want 400", repos, rec.Code)
		}
	}
}

func TestProjectGet_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects/no-such-id", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProjectList_EmptyReturnsArray(t *testing.T) {
	// The handler must return `[]`, not `null` — a frontend that
	// .map()s the response would crash on null.
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

func TestProjectExportPreview_IncludesManifestAndKnowledge(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	s := newTestServer(t)
	id, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "Export me"})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	kbDir := filepath.Join(tempHome, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "notes.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/export/preview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var preview struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	paths := make([]string, 0, len(preview.Files))
	for _, f := range preview.Files {
		paths = append(paths, f.Path)
	}
	if !contains(paths, "manifest.yaml") {
		t.Fatalf("preview is missing manifest.yaml: %v", paths)
	}
	if !contains(paths, "knowledge-base/notes.md") {
		t.Fatalf("preview is missing knowledge file: %v", paths)
	}
}

func TestProjectImport_RoundTripThroughHTTP(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	source := newTestServer(t)
	sourceID, err := source.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{
		Name:        "HTTP Export Source",
		Description: "from source",
	})
	if err != nil {
		t.Fatalf("seed source project: %v", err)
	}
	kbDir := filepath.Join(tempHome, ".triagefactory", "projects", sourceID, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir kb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "readme.md"), []byte("import me"), 0o644); err != nil {
		t.Fatalf("write knowledge file: %v", err)
	}

	exportRec := doJSON(t, source, http.MethodGet, "/api/projects/"+sourceID+"/export", nil)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200; body=%s", exportRec.Code, exportRec.Body.String())
	}
	if got := exportRec.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("export content-type = %q, want application/zip", got)
	}

	target := newTestServer(t)
	importRec := doBundleImport(t, target, exportRec.Body.Bytes())
	if importRec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201; body=%s", importRec.Code, importRec.Body.String())
	}
	var body struct {
		Project  domain.Project      `json:"project"`
		Warnings []map[string]string `json:"warnings"`
	}
	if err := json.Unmarshal(importRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode import body: %v", err)
	}
	if body.Project.ID == "" || body.Project.ID == sourceID {
		t.Fatalf("imported project id invalid: %q", body.Project.ID)
	}
	if body.Project.Name != "HTTP Export Source" {
		t.Fatalf("imported project name = %q", body.Project.Name)
	}
	if len(body.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", body.Warnings)
	}
	importedKB := filepath.Join(tempHome, ".triagefactory", "projects", body.Project.ID, "knowledge-base", "readme.md")
	got, err := os.ReadFile(importedKB)
	if err != nil {
		t.Fatalf("read imported knowledge file: %v", err)
	}
	if string(got) != "import me" {
		t.Fatalf("imported knowledge content mismatch: %q", string(got))
	}
}

func TestProjectPatch_PartialFieldsLeaveOthersUnchanged(t *testing.T) {
	s := newTestServer(t)
	id, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{
		Name:        "Original",
		Description: "Original description",
		PinnedRepos: []string{"a/b"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"description": "Updated description",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Name != "Original" {
		t.Errorf("name changed unexpectedly: %q", got.Name)
	}
	if got.Description != "Updated description" {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "a/b" {
		t.Errorf("pinned_repos changed unexpectedly: %v", got.PinnedRepos)
	}
}

// TestProjectPatch_PinnedReposExplicitEmptyClears confirms a client
// can clear pinned_repos by sending []. The pointer-typed *[]string
// distinguishes "absent (leave alone)" from "explicit empty (clear)";
// without that distinction the handler couldn't tell the cases apart.
func TestProjectPatch_PinnedReposExplicitEmptyClears(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "P", PinnedRepos: []string{"a/b"}})

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got, _ := s.projects.Get(t.Context(), runmode.LocalDefaultOrg, id)
	if len(got.PinnedRepos) != 0 {
		t.Errorf("pinned_repos should be empty, got %v", got.PinnedRepos)
	}
}

func TestProjectPatch_RejectsEmptyName(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{"name": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestProjectPatch_SpecBlueprintAcceptsVisible: setting
// spec_authorship_blueprint_id to a blueprint the project's team can see (here
// a same-team user blueprint) succeeds. The validation runs the same
// List(team) predicate the picker uses, so anything the picker offers is
// accepted.
func TestProjectPatch_SpecBlueprintAcceptsVisible(t *testing.T) {
	s := newTestServer(t)
	if err := s.blueprints.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID,
		domain.Blueprint{ID: "spec-ok", Name: "Spec", Source: "user", TeamID: runmode.LocalDefaultTeamID}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"spec_authorship_blueprint_id": "spec-ok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.SpecAuthorshipBlueprintID != "spec-ok" {
		t.Errorf("spec_authorship_blueprint_id = %q, want %q", got.SpecAuthorshipBlueprintID, "spec-ok")
	}
}

// TestProjectPatch_SpecBlueprintRejectsInvisible: an id the project's team
// can't see (here simply a non-existent one) is a 400. In multi mode the
// same List(team) predicate rejects another team's blueprint — the cross-team
// guard — which the store-level List-narrowing pgtest covers directly; here
// we pin the handler wiring and the 400 on the SQLite path.
func TestProjectPatch_SpecBlueprintRejectsInvisible(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"spec_authorship_blueprint_id": "ghost-id-not-a-real-prompt",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestProjectPatch_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/no-such-id", map[string]any{"name": "X"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProjectDelete_Happy(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "doomed"})

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got, _ := s.projects.Get(t.Context(), runmode.LocalDefaultOrg, id)
	if got != nil {
		t.Errorf("project still readable after delete")
	}
}

func TestProjectDelete_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/no-such-id", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestProjectDelete_RemovesKnowledgeDir verifies the handler clears
// the on-disk knowledge dir. Tested via a fake HOME so we don't
// touch the real ~/.triagefactory.
func TestProjectDelete_RemovesKnowledgeDir(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "with-files"})

	dir := filepath.Join(tempHome, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("notes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	stillThere := filepath.Join(tempHome, ".triagefactory", "projects", id)
	if _, err := os.Stat(stillThere); !os.IsNotExist(err) {
		t.Errorf("knowledge dir not removed: stat err = %v", err)
	}
}

// TestProjectDelete_MissingKnowledgeDir_NoError pins the
// "delete is best-effort on disk" contract: a project with no
// on-disk artifacts must still 204, not 500.
func TestProjectDelete_MissingKnowledgeDir_NoError(t *testing.T) {
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "no-files"})
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

// TestProjectDelete_PathResolutionFailure_StillWarns pins the
// no-silent-skip contract: if projectKnowledgeDir errors (e.g.
// UserHomeDir fails), the handler must still log + set the
// X-Cleanup-Warning header so the client knows on-disk state may
// be stale. Forces UserHomeDir to fail by clearing HOME (and on
// macOS, the user/$USER fallbacks).
func TestProjectDelete_PathResolutionFailure_StillWarns(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")

	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("X-Cleanup-Warning") == "" {
		t.Error("expected X-Cleanup-Warning header when path resolution fails; got empty")
	}
}

// TestProjectDelete_CleanupWarningRedactsPath pins the path-leak
// fix: when on-disk cleanup fails, the X-Cleanup-Warning header
// must be a generic message, not rmErr.Error() (which would
// include absolute paths and OS-specific detail). Forces failure
// by dropping write perms on the parent dir so RemoveAll can't
// clear the contents.
func TestProjectDelete_CleanupWarningRedactsPath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "padded"})

	dir := filepath.Join(tempHome, ".triagefactory", "projects", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret-path-leak.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drop write perms on the dir so RemoveAll fails to delete the
	// child. Restore in cleanup so t.TempDir's own cleanup works.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	warning := rec.Header().Get("X-Cleanup-Warning")
	if warning == "" {
		t.Fatal("expected X-Cleanup-Warning header, got empty")
	}
	if strings.Contains(warning, tempHome) || strings.Contains(warning, dir) {
		t.Errorf("warning leaks filesystem path: %q", warning)
	}
	if strings.Contains(warning, "secret-path-leak.md") {
		t.Errorf("warning leaks filename: %q", warning)
	}
}

func TestValidatePinnedRepoShape_Slugs(t *testing.T) {
	good := [][]string{
		nil,
		{},
		{"a/b"},
		{"sky-ai-eng/triage-factory", "owner/repo"},
	}
	for _, repos := range good {
		if _, errMsg := validatePinnedRepoShape(repos); errMsg != "" {
			t.Errorf("repos=%v should pass, got %q", repos, errMsg)
		}
	}
	bad := [][]string{
		{""},
		{"  "},
		{"justaword"},
		{"a/b/c"},
		{"/x"},
		{"x/"},
	}
	for _, repos := range bad {
		if _, errMsg := validatePinnedRepoShape(repos); errMsg == "" {
			t.Errorf("repos=%v should reject", repos)
		}
	}
}

// TestValidatePinnedRepoShape_NormalizesWhitespace pins the
// trim-and-persist contract: validation strips whitespace AND the
// caller persists the trimmed slugs. Without normalization,
// " owner/repo " would pass (validator trims) but get stored
// padded, breaking later lookups by slug.
func TestValidatePinnedRepoShape_NormalizesWhitespace(t *testing.T) {
	in := []string{"  owner/repo  ", "\tother/repo\n"}
	out, errMsg := validatePinnedRepoShape(in)
	if errMsg != "" {
		t.Fatalf("expected pass, got %q", errMsg)
	}
	want := []string{"owner/repo", "other/repo"}
	if len(out) != len(want) {
		t.Fatalf("len = %d, want %d", len(out), len(want))
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("[%d] = %q, want %q", i, out[i], w)
		}
	}
}

// TestValidatePinnedRepos_RejectsUnconfigured pins the existence-check
// contract: a slug that's well-formed but isn't tracked by the project's
// team (team_github_repos) is rejected at the API layer. This is what
// stops a curl-crafted POST from pinning a repo the team has never set up
// (no creds, no clone URL, nothing for the Curator to materialize).
func TestValidatePinnedRepos_RejectsUnconfigured(t *testing.T) {
	srv := newTestServer(t)
	seedConfiguredRepo(t, srv, "sky-ai-eng", "configured")

	ctx := t.Context()
	teamRepos := sqlitestore.New(srv.db).TeamGitHubRepos
	teamID := runmode.LocalDefaultTeamID

	// All-tracked passes.
	if _, errMsg := validatePinnedRepos(ctx, teamRepos, teamID, []string{"sky-ai-eng/configured"}); errMsg != "" {
		t.Errorf("tracked slug should pass, got %q", errMsg)
	}

	// Mix of tracked + untracked rejects on the untracked one.
	if _, errMsg := validatePinnedRepos(ctx, teamRepos, teamID, []string{"sky-ai-eng/configured", "stranger/repo"}); errMsg == "" {
		t.Error("untracked slug should reject")
	} else if !strings.Contains(errMsg, "stranger/repo") {
		t.Errorf("error should name the offending slug, got %q", errMsg)
	}

	// Empty input still passes (nothing to check).
	if _, errMsg := validatePinnedRepos(ctx, teamRepos, teamID, nil); errMsg != "" {
		t.Errorf("nil input should pass, got %q", errMsg)
	}

	// Casing mismatch still passes: GitHub owner/repo names are
	// case-insensitive and the rest of SKY-375 folds case, so a pin whose
	// capitalization differs from the tracked row (e.g. after the repo was
	// re-saved with different casing) is the same repo, not an untracked one.
	if _, errMsg := validatePinnedRepos(ctx, teamRepos, teamID, []string{"Sky-AI-Eng/Configured"}); errMsg != "" {
		t.Errorf("case-variant of a tracked slug should pass, got %q", errMsg)
	}
}

// TestProjectCreate_PaddedSlugsStoredTrimmed is the end-to-end
// regression: padded input from a client must round-trip back as
// trimmed. Without the normalization fix this test fails because
// the original padded string gets persisted.
func TestProjectCreate_PaddedSlugsStoredTrimmed(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":         "P",
		"pinned_repos": []string{"  owner/repo  "},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "owner/repo" {
		t.Errorf("pinned_repos = %v, want [\"owner/repo\"]", got.PinnedRepos)
	}
}

func TestProjectPatch_PaddedSlugsStoredTrimmed(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "only", "one")
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{" \tonly/one  "},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got, _ := s.projects.Get(t.Context(), runmode.LocalDefaultOrg, id)
	if len(got.PinnedRepos) != 1 || got.PinnedRepos[0] != "only/one" {
		t.Errorf("pinned_repos = %v, want [\"only/one\"]", got.PinnedRepos)
	}
}

// TestValidateTrackerKeys_AcceptsConfigured verifies that a Jira
// project key already in the team's rules is accepted as-is. This
// is the happy path the create modal hits when a user picks a
// project from the Settings-curated list.
func TestValidateTrackerKeys_AcceptsConfigured(t *testing.T) {
	rules := []domain.JiraProjectStatusRules{{ProjectKey: "SKY"}, {ProjectKey: "OPS"}}
	jira, linear, errMsg := validateTrackerKeys(rules, "SKY", "")
	if errMsg != "" {
		t.Fatalf("expected no error, got %q", errMsg)
	}
	if jira != "SKY" {
		t.Errorf("jira = %q, want SKY", jira)
	}
	if linear != "" {
		t.Errorf("linear = %q, want empty", linear)
	}
}

// TestValidateTrackerKeys_RejectsUnconfigured exercises the SKY-217
// contract: a Jira key not in the team's rules gets rejected with a
// message pointing at Settings. Stale clients (project removed from
// config after pinning) and curl users both hit this path.
func TestValidateTrackerKeys_RejectsUnconfigured(t *testing.T) {
	rules := []domain.JiraProjectStatusRules{{ProjectKey: "SKY"}}
	_, _, errMsg := validateTrackerKeys(rules, "OPS", "")
	if errMsg == "" {
		t.Fatal("expected error for unconfigured Jira key")
	}
	if !strings.Contains(errMsg, "OPS") {
		t.Errorf("error should name the offending key, got %q", errMsg)
	}
	if !strings.Contains(errMsg, "Settings") {
		t.Errorf("error should point at Settings, got %q", errMsg)
	}
}

// TestValidateTrackerKeys_RejectsLinear pins the "Linear is future
// work" decision: any non-empty Linear key is rejected outright.
// Once Linear integration ships this assertion will need to flip.
func TestValidateTrackerKeys_RejectsLinear(t *testing.T) {
	_, _, errMsg := validateTrackerKeys(nil, "", "TF")
	if errMsg == "" {
		t.Fatal("expected error for non-empty Linear key")
	}
	if !strings.Contains(errMsg, "Linear") {
		t.Errorf("error should mention Linear, got %q", errMsg)
	}
}

// TestValidateTrackerKeys_EmptyAcceptsBoth covers the "no tracker"
// case — the user creates a project without picking either tracker.
// Validation should pass with empty normalized values.
func TestValidateTrackerKeys_EmptyAcceptsBoth(t *testing.T) {
	jira, linear, errMsg := validateTrackerKeys(nil, "", "")
	if errMsg != "" {
		t.Fatalf("empty input should pass, got %q", errMsg)
	}
	if jira != "" || linear != "" {
		t.Errorf("expected both empty, got jira=%q linear=%q", jira, linear)
	}
}

// TestValidateTrackerKeys_TrimsWhitespace mirrors the pinned-repos
// whitespace handling: stray padding from a client passes validation
// in normalized form rather than getting stored padded.
func TestValidateTrackerKeys_TrimsWhitespace(t *testing.T) {
	rules := []domain.JiraProjectStatusRules{{ProjectKey: "SKY"}}
	jira, _, errMsg := validateTrackerKeys(rules, "  SKY  ", "")
	if errMsg != "" {
		t.Fatalf("padded input should validate, got %q", errMsg)
	}
	if jira != "SKY" {
		t.Errorf("jira = %q, want trimmed SKY", jira)
	}
}

// TestProjectKnowledge_404OnMissing covers the GET endpoint's
// project-existence guard. A bogus id is a 404, distinct from the
// "exists but no knowledge yet" empty-array path.
func TestProjectKnowledge_404OnMissing(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/projects/no-such-id/knowledge", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestProjectKnowledge_EmptyForFreshProject pins the "no knowledge
// yet ≠ error" UX contract: a project that hasn't been chatted with
// has no knowledge-base subdir, but the endpoint still returns []
// rather than 500-ing. The frontend renders an empty state from
// this response.
func TestProjectKnowledge_EmptyForFreshProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "fresh"})
	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

// TestProjectKnowledge_ReturnsAllFileTypes pins the post-SKY-217
// "knowledge base accepts anything Claude Code can read" contract:
// the listing surfaces every regular file under knowledge-base/,
// detects mime type per extension, and inlines content only for
// text-shaped files. Images and similar binaries surface with a
// mime type and size but no inline content — the frontend lazily
// fetches them via the per-file raw endpoint.
func TestProjectKnowledge_ReturnsAllFileTypes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "with-knowledge"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir knowledge-base: %v", err)
	}
	// Cover the four render branches: markdown, plain text, image
	// (binary), and JSON (text-shaped application/* type).
	files := map[string][]byte{
		"alpha.md":    []byte("# Alpha\n"),
		"notes.txt":   []byte("plain notes"),
		"data.json":   []byte(`{"k":1}`),
		"diagram.png": {0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, // PNG magic
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(kbDir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got []knowledgeFile
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d files, want 4 (one per extension)", len(got))
	}
	byPath := make(map[string]knowledgeFile, len(got))
	for _, f := range got {
		byPath[f.Path] = f
	}

	if md := byPath["alpha.md"]; !strings.HasPrefix(md.MimeType, "text/markdown") {
		t.Errorf("alpha.md mime = %q, want text/markdown*", md.MimeType)
	} else if md.Content != "# Alpha\n" {
		t.Errorf("alpha.md content = %q (text-shaped should inline)", md.Content)
	}

	if txt := byPath["notes.txt"]; !strings.HasPrefix(txt.MimeType, "text/plain") {
		t.Errorf("notes.txt mime = %q, want text/plain*", txt.MimeType)
	} else if txt.Content != "plain notes" {
		t.Errorf("notes.txt content = %q", txt.Content)
	}

	if j := byPath["data.json"]; !strings.HasPrefix(j.MimeType, "application/json") {
		t.Errorf("data.json mime = %q, want application/json", j.MimeType)
	} else if j.Content != `{"k":1}` {
		t.Errorf("data.json content = %q (JSON should inline)", j.Content)
	}

	if png := byPath["diagram.png"]; png.MimeType != "image/png" {
		t.Errorf("diagram.png mime = %q, want image/png", png.MimeType)
	} else if png.Content != "" {
		t.Errorf("diagram.png content = %q, want empty (binary not inlined)", png.Content)
	}
}

// TestProjectKnowledge_LargeTextNotInlined gates the inline-size
// limit. A text file over knowledgeInlineMaxBytes shows up in the
// listing with metadata but no Content — frontend lazy-fetches the
// body when the user expands the file.
func TestProjectKnowledge_LargeTextNotInlined(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "with-big-file"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := bytes.Repeat([]byte("x"), knowledgeInlineMaxBytes+1)
	if err := os.WriteFile(filepath.Join(kbDir, "big.md"), big, 0o644); err != nil {
		t.Fatalf("write big.md: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []knowledgeFile
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 1 {
		t.Fatalf("got %d files", len(got))
	}
	if got[0].Content != "" {
		t.Errorf("oversize text should not inline content (got %d bytes)", len(got[0].Content))
	}
	if got[0].SizeBytes != int64(len(big)) {
		t.Errorf("size = %d, want %d", got[0].SizeBytes, len(big))
	}
}

// TestProjectDelete_404sDuringConcurrentPatch pins the
// PATCH-vs-DELETE serialization. Without the per-project mutex on
// DELETE, an in-flight PATCH would see the row vanish and surface
// sql.ErrNoRows as a 500. With the mutex, the DELETE waits and the
// next PATCH (if any) cleanly 404s.
func TestProjectDelete_404sDuringConcurrentPatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)
	id, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "racy"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Delete the project, then try a PATCH against it. The PATCH
	// should 404, not 500. (We can't reliably exercise the actual
	// race in a unit test without injecting a delay; this asserts
	// the post-delete behavior, which is the user-visible contract.)
	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	desc := "anything"
	rec = doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"description": desc,
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH after DELETE status = %d, want 404", rec.Code)
	}
}

// TestSanitizeKnowledgeFilename_BackslashNotSeparatorOnUnix pins the
// platform-correct path stripping. On Unix '\\' is a literal char,
// not a separator, so a filename like `a\b.md` must round-trip
// intact through sanitization. The earlier version of this code did
// a manual ReplaceAll('\\', '/') which silently shortened that to
// `b.md`, leaving the listing entry no longer resolvable by raw or
// delete endpoints.
func TestSanitizeKnowledgeFilename_BackslashNotSeparatorOnUnix(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("test asserts unix-style separator behavior")
	}
	got, errMsg := sanitizeKnowledgeFilename("a\\b.md")
	if errMsg != "" {
		t.Fatalf("expected to accept literal backslash, got %q", errMsg)
	}
	if got != "a\\b.md" {
		t.Errorf("got %q, want literal a\\b.md", got)
	}
}

// TestProjectKnowledge_HidesUnsanitizableNames pins the filter that
// keeps the listing in sync with what the per-file endpoints will
// accept. A `.cache.json` file written directly by the agent has a
// leading dot and would be rejected by the raw/delete endpoints, so
// we skip it from the listing too — surfacing it would leave the
// user with phantom entries they can't open or remove.
func TestProjectKnowledge_HidesUnsanitizableNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "filter"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "ok.md"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write ok.md: %v", err)
	}
	// Leading-dot file — passes through the OS but would be rejected
	// by sanitizeKnowledgeFilename in the raw/delete endpoints.
	if err := os.WriteFile(filepath.Join(kbDir, ".cache.json"), []byte("[]"), 0o644); err != nil {
		t.Fatalf("write .cache.json: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []knowledgeFile
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Path != "ok.md" {
		t.Errorf("expected only [ok.md], got %d entries: %v", len(got), got)
	}
}

// TestProjectKnowledge_SkipsSymlinks pins the symlink-skipping
// defense. A malicious or careless upload that drops a symlink
// pointing at ~/.ssh/id_rsa would otherwise be readable through
// the knowledge listing; the readKnowledgeFiles loop skips
// symlinks specifically to close that path.
func TestProjectKnowledge_SkipsSymlinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "symlink-test"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "real.md"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	// Symlink pointing to a sibling outside the listing's domain.
	target := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(target, []byte("don't read me"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(kbDir, "leak.md")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []knowledgeFile
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Path != "real.md" {
		t.Errorf("expected only [real.md], got %d entries", len(got))
	}
}

// TestSanitizeKnowledgeFilename pins the bad-input table. Adding a
// case here is the cheap way to guard the upload + delete + raw-fetch
// paths against a new attack class without re-running an HTTP
// roundtrip per case.
func TestSanitizeKnowledgeFilename(t *testing.T) {
	cases := []struct {
		in   string
		want string // empty = expect rejection
	}{
		{"notes.md", "notes.md"},
		{"  notes.md  ", "notes.md"},
		{"sub/dir/notes.md", "notes.md"},       // path stripped to base
		{"..", ""},                             // dotdot rejected
		{".", ""},                              // dot rejected
		{"", ""},                               // empty rejected
		{".hidden", ""},                        // leading dot rejected
		{"folder\\file.md", "folder\\file.md"}, // unix: literal char (filepath.Base no-op)
		{"a/../b.md", "b.md"},                  // base of "../b.md" is "b.md"
		{string([]byte{}), ""},                 // truly empty
	}
	for _, tc := range cases {
		got, errMsg := sanitizeKnowledgeFilename(tc.in)
		if tc.want == "" {
			if errMsg == "" {
				t.Errorf("input %q expected rejection, got %q", tc.in, got)
			}
			continue
		}
		if errMsg != "" {
			t.Errorf("input %q expected %q, rejected with %q", tc.in, tc.want, errMsg)
			continue
		}
		if got != tc.want {
			t.Errorf("input %q got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestProjectKnowledgeUpload_Happy verifies the multipart upload
// path: a single file lands at <kbDir>/<original-name> and shows up
// in the subsequent listing.
func TestProjectKnowledgeUpload_Happy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "uploads"})

	rec := doMultipartUpload(t, s, "/api/projects/"+id+"/knowledge", map[string][]byte{
		"hello.md": []byte("# hello\n"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	full := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base", "hello.md")
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(body) != "# hello\n" {
		t.Errorf("uploaded content = %q", string(body))
	}
}

// TestProjectKnowledgeUpload_RejectsConflict pins the "conflict =
// reject" decision. Re-uploading the same name without first
// deleting surfaces an error in the per-file results, while siblings
// in the same request continue independently.
func TestProjectKnowledgeUpload_RejectsConflict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "conflicts"})

	// Pre-seed an existing file.
	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "notes.md"), []byte("original"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doMultipartUpload(t, s, "/api/projects/"+id+"/knowledge", map[string][]byte{
		"notes.md":   []byte("would-overwrite"),
		"sibling.md": []byte("ok"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Results []struct {
			Path     string `json:"path"`
			Original string `json:"original"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(resp.Results))
	}
	var conflictErr, siblingPath string
	for _, r := range resp.Results {
		if r.Original == "notes.md" {
			conflictErr = r.Error
		}
		if r.Original == "sibling.md" {
			siblingPath = r.Path
		}
	}
	if conflictErr == "" {
		t.Error("expected conflict error on notes.md")
	}
	if siblingPath != "sibling.md" {
		t.Errorf("expected sibling to succeed, got path=%q", siblingPath)
	}

	// Original content must be untouched after the rejected overwrite.
	body, _ := os.ReadFile(filepath.Join(kbDir, "notes.md"))
	if string(body) != "original" {
		t.Errorf("conflict overwrote: got %q, want %q", body, "original")
	}
}

// TestProjectKnowledgeUpload_SizeLimit verifies the per-file cap.
// A blob larger than knowledgeMaxUploadBytes gets rejected with no
// partial file left on disk.
func TestProjectKnowledgeUpload_SizeLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "sizecap"})

	huge := bytes.Repeat([]byte("x"), knowledgeMaxUploadBytes+10)
	rec := doMultipartUpload(t, s, "/api/projects/"+id+"/knowledge", map[string][]byte{
		"big.bin": huge,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Results []struct {
			Original string `json:"original"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Results) != 1 || resp.Results[0].Error == "" {
		t.Fatalf("expected size-limit rejection, got %+v", resp.Results)
	}
	full := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base", "big.bin")
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Errorf("rejected upload left a file on disk")
	}
}

// TestProjectKnowledgeFile_StreamsRaw verifies the per-file raw
// endpoint serves the bytes with the correct Content-Type header.
// This is what the frontend's <img src=> tags hit for image
// previews.
func TestProjectKnowledgeFile_StreamsRaw(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "raw"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00}
	if err := os.WriteFile(filepath.Join(kbDir, "logo.png"), pngBytes, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge/logo.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), pngBytes) {
		t.Errorf("body bytes don't round-trip")
	}
}

// TestProjectKnowledgeFile_RejectsSymlink pins the per-file
// symlink defense. The listing endpoint already skips symlinks; the
// raw-fetch endpoint has to close the same hole independently
// because os.Stat would happily follow a symlink whose target is a
// regular file outside knowledge-base/, exposing arbitrary bytes
// from the user's home directory.
func TestProjectKnowledgeFile_RejectsSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "symlink-fetch"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(target, []byte("don't expose me"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(kbDir, "leak.txt")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge/leak.txt", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (symlink rejected)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "don't expose me") {
		t.Errorf("response body leaked target file contents")
	}
}

// TestProjectKnowledgeFile_RejectsTraversal pins the path-traversal
// defense. Even URL-encoded "../config.yaml" surfaces as a 400, not
// a successful read of a sensitive file outside the knowledge-base.
func TestProjectKnowledgeFile_RejectsTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "traversal"})

	// URL-encoded forms bypass net/http's path cleanup and reach the
	// handler intact — that's where our resolveKnowledgePath defense
	// has to actually fire. Literal ".." is handled upstream by the
	// mux redirecting to the cleaned path, which we accept as
	// equivalent (the file never gets read either way).
	bad := []string{
		"%2E%2E",               // url-encoded ".."
		"%2E%2E%2Fconfig.yaml", // url-encoded "../config.yaml"
		".hidden",              // leading dot
	}
	for _, p := range bad {
		rec := doJSON(t, s, http.MethodGet, "/api/projects/"+id+"/knowledge/"+p, nil)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("path %q got status %d, want 400 or 404", p, rec.Code)
		}
	}
}

// TestProjectKnowledgeDelete_RemovesFile verifies the delete endpoint
// pairs cleanly with upload — a file uploaded then deleted no longer
// appears in the listing.
func TestProjectKnowledgeDelete_RemovesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "delete"})

	kbDir := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	full := filepath.Join(kbDir, "doomed.md")
	if err := os.WriteFile(full, []byte("bye"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/projects/"+id+"/knowledge/doomed.md", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Errorf("file still on disk after delete")
	}

	// Second delete returns 404.
	rec = doJSON(t, s, http.MethodDelete, "/api/projects/"+id+"/knowledge/doomed.md", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("re-delete status = %d, want 404", rec.Code)
	}
}

// TestProjectCreate_AcceptsTrackerKeys verifies the create handler's
// tracker validation is wired up. We seed a configured Jira project
// rule via the store and confirm the create flow stores the key and
// read-back returns it.
func TestProjectCreate_AcceptsTrackerKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)
	if err := s.jiraRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, []domain.JiraProjectStatusRules{
		validProjectRule("SKY"),
	}); err != nil {
		t.Fatalf("save jira rules: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":             "Tracked",
		"jira_project_key": "SKY",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got domain.Project
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.JiraProjectKey != "SKY" {
		t.Errorf("jira_project_key = %q, want SKY", got.JiraProjectKey)
	}
}

// TestProjectCreate_RejectsUnconfiguredJira verifies the e2e
// validation path. Without the Jira key in config, create should
// 400 with a hint pointing the user at Settings.
func TestProjectCreate_RejectsUnconfiguredJira(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/projects", map[string]any{
		"name":             "Bogus",
		"jira_project_key": "NOPE",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

// TestProjectPatch_PartialTrackerUpdateValidatesOnlyChangedSide
// covers the "PATCH only the field the client sent" contract. A
// project pre-populated with a now-invalid Jira key (e.g. config
// drifted after creation) can still have its Linear key cleared
// without re-validating the stale Jira side.
func TestProjectPatch_PartialTrackerUpdateValidatesOnlyChangedSide(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newTestServer(t)
	id, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{
		Name:           "Drifted",
		JiraProjectKey: "STALE", // not in (empty) config — set directly via DB
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// PATCH only linear_project_key (clearing it). Should succeed
	// despite the Jira side being out of sync with config.
	empty := ""
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"linear_project_key": empty,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, _ := s.projects.Get(t.Context(), runmode.LocalDefaultOrg, id)
	if got.JiraProjectKey != "STALE" {
		t.Errorf("jira preserved = %q, want STALE", got.JiraProjectKey)
	}
}
