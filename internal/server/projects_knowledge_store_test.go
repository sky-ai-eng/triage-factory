package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

func TestParseSingleByteRange(t *testing.T) {
	const size = 10
	cases := []struct {
		header    string
		wantStart int64
		wantLen   int64
		wantOK    bool
	}{
		{"bytes=0-", 0, 10, true},
		{"bytes=5-9", 5, 5, true},
		{"bytes=5-", 5, 5, true},
		{"bytes=2-4", 2, 3, true},
		{"bytes=-3", 7, 3, true},       // suffix: last 3 bytes
		{"bytes=-100", 0, 10, true},    // suffix larger than size clamps to whole
		{"bytes=5-100", 5, 5, true},    // end past size clamps to last byte
		{"", 0, 0, false},              // absent
		{"bytes=", 0, 0, false},        // empty spec
		{"bytes=abc", 0, 0, false},     // malformed
		{"bytes=10-", 0, 0, false},     // start at size: unsatisfiable → 200
		{"bytes=9-5", 0, 0, false},     // end before start
		{"bytes=0-1,4-5", 0, 0, false}, // multi-range → 200
		{"items=0-1", 0, 0, false},     // wrong unit
	}
	for _, tc := range cases {
		start, length, ok := parseSingleByteRange(tc.header, size)
		if ok != tc.wantOK || (ok && (start != tc.wantStart || length != tc.wantLen)) {
			t.Errorf("parseSingleByteRange(%q, %d) = (%d, %d, %v); want (%d, %d, %v)",
				tc.header, size, start, length, ok, tc.wantStart, tc.wantLen, tc.wantOK)
		}
	}
}

// newKBTestServer builds a Server with an fs-backed KB store and seeds files
// straight into it, so the store-backed handler helpers can be exercised
// without the DB/lock/auth machinery the full HTTP path needs.
func newKBTestServer(t *testing.T) (*Server, *kbstore.Store) {
	t.Helper()
	s := newTestServer(t)
	kb := kbstore.New(storage.NewFS(t.TempDir()))
	s.SetKBStore(kb)
	return s, kb
}

func TestServeKnowledgeFromStore_FullAnd206(t *testing.T) {
	s, kb := newKBTestServer(t)
	ctx := context.Background()
	const org, proj = "o", "p"
	body := "0123456789abcdef"
	if err := kb.Put(ctx, org, proj, "clip.bin", strings.NewReader(body)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Full stream → 200 with the whole body.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	s.serveKnowledgeFromStore(rec, req, org, proj, "clip.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("full serve = %d; want 200", rec.Code)
	}
	if rec.Body.String() != body {
		t.Fatalf("full body = %q; want %q", rec.Body.String(), body)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q; want bytes", ar)
	}

	// Single range → 206 with Content-Range + the requested slice.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=5-9")
	rec = httptest.NewRecorder()
	s.serveKnowledgeFromStore(rec, req, org, proj, "clip.bin")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range serve = %d; want 206", rec.Code)
	}
	if got := rec.Body.String(); got != body[5:10] {
		t.Fatalf("range body = %q; want %q", got, body[5:10])
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 5-9/16" {
		t.Errorf("Content-Range = %q; want bytes 5-9/16", cr)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "5" {
		t.Errorf("Content-Length = %q; want 5", cl)
	}

	// Missing file → 404.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	rec = httptest.NewRecorder()
	s.serveKnowledgeFromStore(rec, req, org, proj, "nope.bin")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing serve = %d; want 404", rec.Code)
	}
}

func TestListKnowledgeFromStore_InlinesText(t *testing.T) {
	s, kb := newKBTestServer(t)
	ctx := context.Background()
	const org, proj = "o", "p"
	if err := kb.Put(ctx, org, proj, "notes.md", strings.NewReader("# hello")); err != nil {
		t.Fatalf("seed md: %v", err)
	}
	// A non-text file: content must NOT be inlined.
	if err := kb.Put(ctx, org, proj, "pic.png", strings.NewReader("\x89PNGbinary")); err != nil {
		t.Fatalf("seed png: %v", err)
	}

	files, err := s.listKnowledgeFromStore(ctx, org, proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("list len = %d; want 2 (%+v)", len(files), files)
	}
	byName := map[string]knowledgeFile{}
	for _, f := range files {
		byName[f.Path] = f
	}
	if md := byName["notes.md"]; md.Content != "# hello" {
		t.Errorf("notes.md content = %q; want inlined", md.Content)
	}
	if !strings.HasPrefix(byName["notes.md"].MimeType, "text/markdown") {
		t.Errorf("notes.md mime = %q; want markdown", byName["notes.md"].MimeType)
	}
	if png := byName["pic.png"]; png.Content != "" {
		t.Errorf("pic.png content = %q; want empty (binary lazy-fetch)", png.Content)
	}
	if byName["pic.png"].SizeBytes != int64(len("\x89PNGbinary")) {
		t.Errorf("pic.png size = %d; want %d", byName["pic.png"].SizeBytes, len("\x89PNGbinary"))
	}
}
