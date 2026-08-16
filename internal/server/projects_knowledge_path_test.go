package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// doRaw issues a request with the path taken verbatim (no re-encoding), so a
// test can pin exactly what the client put on the wire.
func doRaw(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestProjectKnowledge_PercentNamesRoundTrip pins the listing and the
// per-file routes agreeing on one encoding. The mux already
// percent-decodes {path}; when the handler decoded it a second time, a file
// listed as "notes%20final.md" and requested the way any client encodes it
// (once, encodeURIComponent-style) arrived as "notes final.md" and 404'd,
// and "100%.md" failed the second decode as a 400. Upload → list → GET →
// DELETE, using the exact listed name encoded exactly once.
func TestProjectKnowledge_PercentNamesRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "percent-names"})

	uploads := map[string][]byte{
		"100%.md":          []byte("# hundred percent\n"),
		"notes%20final.md": []byte("# not a space\n"),
	}
	rec := doMultipartUpload(t, s, "/api/projects/"+id+"/knowledge", uploads)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body=%s", rec.Code, rec.Body.String())
	}

	// The listing is the client's source of truth for names.
	listed := listKnowledge(t, s, id)
	names := make(map[string]bool, len(listed))
	for _, f := range listed {
		names[f.Path] = true
	}
	for name := range uploads {
		if !names[name] {
			t.Fatalf("listing %v is missing %q", names, name)
		}
	}

	for name, want := range uploads {
		// url.PathEscape matches encodeURIComponent for these names: one
		// encoding pass, "%" → "%25".
		target := "/api/projects/" + id + "/knowledge/" + url.PathEscape(name)

		got := doRaw(t, s, http.MethodGet, target)
		if got.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body=%s", target, got.Code, got.Body.String())
		}
		if got.Body.String() != string(want) {
			t.Errorf("GET %s body = %q, want %q", target, got.Body.String(), want)
		}

		del := doRaw(t, s, http.MethodDelete, target)
		if del.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d, want 204; body=%s", target, del.Code, del.Body.String())
		}
		if _, err := os.Stat(filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base", name)); !os.IsNotExist(err) {
			t.Errorf("%q still on disk after DELETE (stat err = %v)", name, err)
		}
	}
}

// TestProjectKnowledge_EncodedSlashResolvesNothing pins the safe behavior for
// an encoded separator. The mux hands "a%2Fb.md" to the handler as "a/b.md";
// that names no knowledge file (the knowledge base is flat), and it must
// never be read as a path whose last component happens to name a real one.
func TestProjectKnowledge_EncodedSlashResolvesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	id, _ := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "encoded-slash"})

	if rec := doMultipartUpload(t, s, "/api/projects/"+id+"/knowledge", map[string][]byte{
		"b.md": []byte("secret-ish\n"),
	}); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body=%s", rec.Code, rec.Body.String())
	}

	target := "/api/projects/" + id + "/knowledge/a%2Fb.md"
	got := doRaw(t, s, http.MethodGet, target)
	if got.Code == http.StatusOK {
		t.Fatalf("GET %s = 200 body=%q; an encoded slash must not resolve to a file", target, got.Body.String())
	}
	if got.Code != http.StatusBadRequest {
		t.Errorf("GET %s = %d, want 400", target, got.Code)
	}

	del := doRaw(t, s, http.MethodDelete, target)
	if del.Code != http.StatusBadRequest {
		t.Errorf("DELETE %s = %d, want 400", target, del.Code)
	}
	onDisk := filepath.Join(home, ".triagefactory", "projects", id, "knowledge-base", "b.md")
	if _, err := os.Stat(onDisk); err != nil {
		t.Errorf("b.md gone after a rejected encoded-slash DELETE: %v", err)
	}
}
