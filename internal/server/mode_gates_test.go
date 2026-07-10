package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestMultiModeGates_Return501 pins the local-only endpoints that read or
// write the orchestrator process's own filesystem (skills scan-import) or
// still run raw SQLite-shaped SQL (project bundle export/preview). Each
// must refuse cleanly in multi mode instead of 500ing or silently
// succeeding with wrong-tree reads.
func TestMultiModeGates_Return501(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := &Server{}
	sk := &skillsHandler{}

	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
	}{
		{"skills import", sk.handleSkillsImport, http.MethodPost, "/api/skills/import"},
		{"project import", s.handleProjectImport, http.MethodPost, "/api/projects/import"},
		{"project export", s.handleProjectExport, http.MethodGet, "/api/projects/p1/export"},
		{"project export preview", s.handleProjectExportPreview, http.MethodGet, "/api/projects/p1/export/preview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: "user-1"})
			ctx = httpx.WithOrgID(ctx, "org-1")
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("%s %s in multi mode = %d, want 501 (body: %s)", tc.method, tc.target, rec.Code, rec.Body.String())
			}
		})
	}
}
