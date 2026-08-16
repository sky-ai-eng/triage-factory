package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// TestKnowledgeMulti_RoundTrip drives the full multi-mode KB handler stack —
// the advisory lock, the DB project read, and the blob store — through
// upload → list → serve(200 + 206) → conflict → delete. It skips cleanly
// without Docker (pgtest), and the KB store is a real fs-backed Storage so the
// store round-trip is exercised end to end.
func TestKnowledgeMulti_RoundTrip(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	s.SetKBStore(kbstore.New(storage.NewFS(t.TempDir())))

	ctx := context.Background()
	org, user, team := pgtest.SeedOrgWithUser(t, h, "kb-multi")
	projectID, err := stores.Projects.Create(ctx, org, team, domain.Project{Name: "KB Project"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	authed := func(req *http.Request) *http.Request {
		c := httpx.WithOrgID(req.Context(), org)
		c = httpx.WithClaims(c, &verify.Claims{Subject: user})
		req = req.WithContext(c)
		req.SetPathValue("id", projectID)
		return req
	}

	clip := []byte("0123456789abcdefghij") // 20 bytes

	// --- Upload note.md (text) + clip.bin (binary) ---
	{
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for name, content := range map[string][]byte{"note.md": []byte("# hello"), "clip.bin": clip} {
			fw, _ := mw.CreateFormFile("file", name)
			fw.Write(content)
		}
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/x", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s.handleProjectKnowledgeUpload(rec, authed(req))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload = %d; want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Results []struct {
				Path  string `json:"path"`
				Error string `json:"error"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode upload resp: %v", err)
		}
		for _, r := range resp.Results {
			if r.Error != "" {
				t.Fatalf("upload result error for %q: %s", r.Path, r.Error)
			}
		}
	}

	// --- List: both present, note.md inlined ---
	{
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleProjectKnowledge(rec, authed(req))
		files := decodeList[knowledgeFile](t, rec).Items
		byName := map[string]knowledgeFile{}
		for _, f := range files {
			byName[f.Path] = f
		}
		if len(byName) != 2 {
			t.Fatalf("list has %d files; want 2 (%+v)", len(byName), files)
		}
		if byName["note.md"].Content != "# hello" {
			t.Errorf("note.md content = %q; want inlined", byName["note.md"].Content)
		}
		if byName["clip.bin"].Content != "" {
			t.Errorf("clip.bin content = %q; want empty (binary)", byName["clip.bin"].Content)
		}
	}

	// --- Serve full (200) ---
	{
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = authed(req)
		req.SetPathValue("path", "clip.bin")
		rec := httptest.NewRecorder()
		s.handleProjectKnowledgeFile(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("serve full = %d; want 200", rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), clip) {
			t.Fatalf("serve full body = %q; want %q", rec.Body.Bytes(), clip)
		}
	}

	// --- Serve range (206) ---
	{
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Range", "bytes=10-14")
		req = authed(req)
		req.SetPathValue("path", "clip.bin")
		rec := httptest.NewRecorder()
		s.handleProjectKnowledgeFile(rec, req)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("serve range = %d; want 206", rec.Code)
		}
		if got := rec.Body.String(); got != string(clip[10:15]) {
			t.Fatalf("serve range body = %q; want %q", got, clip[10:15])
		}
		if cr := rec.Header().Get("Content-Range"); cr != "bytes 10-14/20" {
			t.Errorf("Content-Range = %q; want bytes 10-14/20", cr)
		}
	}

	// --- Upload conflict: note.md again ---
	{
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "note.md")
		fw.Write([]byte("# hello again"))
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/x", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s.handleProjectKnowledgeUpload(rec, authed(req))
		var resp struct {
			Results []struct {
				Error string `json:"error"`
			} `json:"results"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Results) != 1 || resp.Results[0].Error == "" {
			t.Fatalf("conflict upload results = %+v; want one conflict error", resp.Results)
		}
	}

	// --- Delete note.md → 204, then missing → 404 ---
	{
		req := httptest.NewRequest(http.MethodDelete, "/x", nil)
		req = authed(req)
		req.SetPathValue("path", "note.md")
		rec := httptest.NewRecorder()
		s.handleProjectKnowledgeDelete(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete = %d; want 204; body=%s", rec.Code, rec.Body.String())
		}

		req = httptest.NewRequest(http.MethodDelete, "/x", nil)
		req = authed(req)
		req.SetPathValue("path", "note.md")
		rec = httptest.NewRecorder()
		s.handleProjectKnowledgeDelete(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("delete missing = %d; want 404", rec.Code)
		}
	}

	// --- Final list: only clip.bin remains ---
	{
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleProjectKnowledge(rec, authed(req))
		files := decodeList[knowledgeFile](t, rec).Items
		if len(files) != 1 || files[0].Path != "clip.bin" {
			t.Fatalf("final list = %+v; want only clip.bin", files)
		}
	}
}
