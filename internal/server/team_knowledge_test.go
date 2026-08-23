package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The team knowledge base's HTTP surface, in local mode: the shape of the five
// routes and the contract each one owes. The cross-team visibility half — a
// non-member reading another team's published root, and never its private one
// — needs real memberships and lives on the Postgres rig
// (team_knowledge_pg_test.go); N=1 has one team and one user, so there is no
// boundary here to cross.

func kbPath(suffix string) string {
	return "/api/teams/default/kb" + suffix
}

// uploadKnowledge posts a multipart body the way the page does: the root, an
// optional folder prefix, then one part per file.
func uploadKnowledge(t *testing.T, s *Server, root, prefix string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if root != "" {
		if err := mw.WriteField("root", root); err != nil {
			t.Fatalf("write root field: %v", err)
		}
	}
	if prefix != "" {
		if err := mw.WriteField("path_prefix", prefix); err != nil {
			t.Fatalf("write path_prefix field: %v", err)
		}
	}
	for name, content := range files {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create part %s: %v", name, err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatalf("write part %s: %v", name, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, kbPath("/files"), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func mustUpload(t *testing.T, s *Server, root, prefix string, files map[string]string) knowledgeUploadResponse {
	t.Helper()
	rec := uploadKnowledge(t, s, root, prefix, files)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out knowledgeUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return out
}

// listKnowledge posts a list body and decodes the standard envelope.
func listKnowledge(t *testing.T, s *Server, body map[string]any) (items []knowledgeFileJSON, total int) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, kbPath("/list"), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items      []knowledgeFileJSON `json:"items"`
		TotalCount *int                `json:"total_count"`
		Next       string              `json:"next_page_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if out.TotalCount == nil {
		t.Fatalf("list answered no total_count: %s", rec.Body.String())
	}
	return out.Items, *out.TotalCount
}

func refsOf(items []knowledgeFileJSON) []string {
	out := make([]string, len(items))
	for i, f := range items {
		out[i] = f.Root + "/" + f.Path
	}
	return out
}

func TestTeamKnowledge_UploadListReadRoundTrip(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	up := mustUpload(t, s, "private", "runbooks", map[string]string{
		"deploy.md":      "how we deploy",
		"oncall/rota.md": "who is on",
	})
	if len(up.Files) != 2 {
		t.Fatalf("upload reported %d files, want 2: %+v", len(up.Files), up.Files)
	}
	for _, f := range up.Files {
		if f.Outcome != "created" {
			t.Errorf("%s outcome = %q, want created", f.Path, f.Outcome)
		}
	}

	items, total := listKnowledge(t, s, map[string]any{})
	if total != 2 {
		t.Fatalf("total_count = %d, want 2", total)
	}
	want := []string{"private/runbooks/deploy.md", "private/runbooks/oncall/rota.md"}
	if got := refsOf(items); !equalSlices(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	// The leaf name rides the row so a client renders it without re-splitting.
	if items[0].Name != "deploy.md" {
		t.Errorf("name = %q, want deploy.md", items[0].Name)
	}

	// The raw read: addressed by the same "<root>/<path>" a listing renders.
	req := httptest.NewRequest(http.MethodGet, kbPath("/private/runbooks/oncall/rota.md"), nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "who is on" {
		t.Errorf("read body = %q, want %q", rec.Body.String(), "who is on")
	}
	// Never rendered as HTML: the bytes are user-uploaded and same-origin.
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing nosniff on a user-uploaded body")
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Errorf("missing Content-Disposition")
	}
}

func TestTeamKnowledge_UploadReplacesAndSaysSo(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	mustUpload(t, s, "private", "", map[string]string{"guide.md": "v1"})
	again := mustUpload(t, s, "private", "", map[string]string{"guide.md": "v2"})
	if len(again.Files) != 1 || again.Files[0].Outcome != "replaced" {
		t.Fatalf("second upload = %+v; want one file reported as replaced", again.Files)
	}

	req := httptest.NewRequest(http.MethodGet, kbPath("/private/guide.md"), nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Body.String() != "v2" {
		t.Errorf("read after replace = %q, want v2", rec.Body.String())
	}
}

func TestTeamKnowledge_ListFiltersByRootAndPrefix(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	mustUpload(t, s, "private", "notes", map[string]string{"a.md": "a"})
	mustUpload(t, s, "private", "notes-archive", map[string]string{"c.md": "c"})
	mustUpload(t, s, "shared", "", map[string]string{"public.md": "p"})

	items, total := listKnowledge(t, s, map[string]any{"root": "shared"})
	if total != 1 || !equalSlices(refsOf(items), []string{"shared/public.md"}) {
		t.Fatalf("root filter = %v (total %d), want just the shared entry", refsOf(items), total)
	}

	// The prefix matches by segment: notes-archive is a sibling, not a child.
	items, total = listKnowledge(t, s, map[string]any{"path_prefix": "notes"})
	if total != 1 || !equalSlices(refsOf(items), []string{"private/notes/a.md"}) {
		t.Fatalf("prefix filter = %v (total %d), want only notes/", refsOf(items), total)
	}
}

func TestTeamKnowledge_ListPagesAndCountsOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	mustUpload(t, s, "private", "", map[string]string{"a.md": "a", "b.md": "b", "c.md": "c"})

	rec := doJSON(t, s, http.MethodPost, kbPath("/list"), map[string]any{"page_size": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items      []knowledgeFileJSON `json:"items"`
		TotalCount *int                `json:"total_count"`
		Next       string              `json:"next_page_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 2 || *page.TotalCount != 3 || page.Next == "" {
		t.Fatalf("first page = %d items, total %d, next %q; want 2 / 3 / a token",
			len(page.Items), *page.TotalCount, page.Next)
	}

	rec = doJSON(t, s, http.MethodPost, kbPath("/list"), map[string]any{"page_size": 2, "page_token": page.Next})
	var second struct {
		Items []knowledgeFileJSON `json:"items"`
		Next  string              `json:"next_page_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(second.Items) != 1 || second.Next != "" {
		t.Fatalf("second page = %d items, next %q; want 1 and no token", len(second.Items), second.Next)
	}

	// An explicit page_size: 0 is the count-only read.
	items, total := listKnowledge(t, s, map[string]any{"page_size": 0})
	if len(items) != 0 || total != 3 {
		t.Fatalf("count-only read = %d items, total %d; want 0 / 3", len(items), total)
	}
}

func TestTeamKnowledge_MovePublishesAndUnpublishes(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	mustUpload(t, s, "private", "runbooks", map[string]string{
		"deploy.md":      "steps",
		"oncall/rota.md": "who",
	})

	rec := doJSON(t, s, http.MethodPost, kbPath("/move"), map[string]any{
		"from_root": "private", "from_path": "runbooks",
		"to_root": "shared", "to_path": "runbooks",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var moved knowledgeMoveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &moved); err != nil {
		t.Fatalf("decode move: %v", err)
	}
	if moved.Moved != 2 {
		t.Errorf("moved = %d, want 2 (the whole subtree)", moved.Moved)
	}

	items, _ := listKnowledge(t, s, map[string]any{})
	want := []string{"shared/runbooks/deploy.md", "shared/runbooks/oncall/rota.md"}
	if got := refsOf(items); !equalSlices(got, want) {
		t.Fatalf("after publish = %v, want %v", got, want)
	}

	// Unpublish is the same verb the other way.
	rec = doJSON(t, s, http.MethodPost, kbPath("/move"), map[string]any{
		"from_root": "shared", "from_path": "runbooks",
		"to_root": "private", "to_path": "runbooks",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unpublish = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	items, _ = listKnowledge(t, s, map[string]any{})
	if got := refsOf(items); !equalSlices(got, []string{"private/runbooks/deploy.md", "private/runbooks/oncall/rota.md"}) {
		t.Fatalf("after unpublish = %v", got)
	}
}

func TestTeamKnowledge_MoveOntoAnOccupiedPathIsAConflict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	mustUpload(t, s, "private", "", map[string]string{"guide.md": "private version"})
	mustUpload(t, s, "shared", "", map[string]string{"guide.md": "shared version"})

	rec := doJSON(t, s, http.MethodPost, kbPath("/move"), map[string]any{
		"from_root": "private", "from_path": "guide.md",
		"to_root": "shared", "to_path": "guide.md",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("colliding move = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonConflict, "")

	// Nothing moved: both documents are where they were.
	items, _ := listKnowledge(t, s, map[string]any{})
	if got := refsOf(items); !equalSlices(got, []string{"private/guide.md", "shared/guide.md"}) {
		t.Fatalf("after refused move = %v; want both untouched", got)
	}
}

func TestTeamKnowledge_MoveOfNothingIs404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, kbPath("/move"), map[string]any{
		"from_root": "private", "from_path": "nothing.md",
		"to_root": "shared", "to_path": "nothing.md",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("move of nothing = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTeamKnowledge_DeleteRemovesASubtree(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	mustUpload(t, s, "private", "docs", map[string]string{"a.md": "a", "sub/b.md": "b"})
	mustUpload(t, s, "private", "docs-other", map[string]string{"c.md": "c"})

	rec := doJSON(t, s, http.MethodDelete, kbPath("/private/docs"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out knowledgeDeleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if out.Removed != 2 {
		t.Errorf("delete removed %d, want 2", out.Removed)
	}
	items, _ := listKnowledge(t, s, map[string]any{})
	if got := refsOf(items); !equalSlices(got, []string{"private/docs-other/c.md"}) {
		t.Fatalf("after delete = %v; want the sibling folder untouched", got)
	}

	// Deleting the same path again is a 404: there is nothing there to remove,
	// and reporting success would claim work that didn't happen.
	if rec := doJSON(t, s, http.MethodDelete, kbPath("/private/docs"), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}

// TestTeamKnowledge_BadCallsFail walks the validation contract: every fault is
// reported with the field named, nothing is clamped or silently dropped.
func TestTeamKnowledge_BadCallsFail(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	t.Run("unknown list field", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, kbPath("/list"), map[string]any{"roots": "private"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonUnknownField, "roots")
	})

	t.Run("unknown root", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, kbPath("/list"), map[string]any{"root": "public"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "root")
	})

	t.Run("traversal prefix", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, kbPath("/list"), map[string]any{"path_prefix": "../elsewhere"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "path_prefix")
	})

	t.Run("page_size out of range is rejected, not clamped", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, kbPath("/list"), map[string]any{"page_size": 5000})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, "page_size")
	})

	t.Run("move without a destination", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, kbPath("/move"), map[string]any{
			"from_root": "private", "from_path": "a.md",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("move with a traversal destination", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, kbPath("/move"), map[string]any{
			"from_root": "private", "from_path": "a.md",
			"to_root": "shared", "to_path": "../a.md",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "to_path")
	})

	t.Run("upload with no root", func(t *testing.T) {
		rec := uploadKnowledge(t, s, "", "", map[string]string{"a.md": "a"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "root")
	})

	t.Run("upload with an unknown form field", func(t *testing.T) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("visibility", "public")
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, kbPath("/files"), &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "")
	})

	t.Run("upload of a traversal filename", func(t *testing.T) {
		rec := uploadKnowledge(t, s, "private", "", map[string]string{"../escape.md": "x"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "files")
	})

	t.Run("read of a path that is not an address", func(t *testing.T) {
		// A segment that cannot name a document is 404, not 400: naming which
		// part of it was wrong would confirm the shape of names that do exist.
		for _, p := range []string{"/private", "/public/a.md", "/private/.hidden.md"} {
			req := httptest.NewRequest(http.MethodGet, kbPath(p), nil)
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", kbPath(p), rec.Code)
			}
		}
		// A traversal never reaches the handler at all: ServeMux normalizes
		// the request path and redirects, so the cleaned address is what gets
		// routed. Either way the bytes behind ".." are unreachable.
		req := httptest.NewRequest(http.MethodGet, kbPath("/private/../escape.md"), nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("GET a traversal path = 200; it must never resolve")
		}
	})

	t.Run("read of a document that is not there", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, kbPath("/shared/absent.md"), nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("= %d, want 404", rec.Code)
		}
	})
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
