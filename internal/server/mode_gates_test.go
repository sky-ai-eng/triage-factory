package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestMultiModeGates_Return501 pins the local-only endpoints that read
// the orchestrator process's own filesystem. Skills scan-import walks
// this process's ~/.claude/skills, which is only meaningful when the
// process runs on the single trusted user's machine — multi mode must
// refuse cleanly instead of scanning shared-infrastructure $HOME.
// (Project bundle import/export were gated here too until the
// store-layer port made them mode-agnostic.)
func TestMultiModeGates_Return501(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	sk := &skillsHandler{}

	req := httptest.NewRequest(http.MethodPost, "/api/skills/import", nil)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: "user-1"})
	ctx = httpx.WithOrgID(ctx, "org-1")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	sk.handleSkillsImport(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST /api/skills/import in multi mode = %d, want 501 (body: %s)", rec.Code, rec.Body.String())
	}
}
