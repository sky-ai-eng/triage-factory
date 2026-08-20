package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestMultiModeGates_Return404 pins the local-only endpoints that read
// the orchestrator process's own filesystem. Skills scan-import walks
// this process's ~/.claude/skills, which is only meaningful when the
// process runs on the single trusted user's machine — multi mode must
// refuse cleanly instead of scanning shared-infrastructure $HOME. The
// refusal is a 404: a route that doesn't exist in this deployment mode
// answers the same way as one that doesn't exist at all (the marketplace's
// mode gate already did), rather than the 501 this used to give.
// (Project bundle import/export were gated here too until the
// store-layer port made them mode-agnostic.)
func TestMultiModeGates_Return404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	sk := &promptImportHandler{}

	req := httptest.NewRequest(http.MethodPost, "/api/prompts/from-disk", nil)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: "user-1"})
	ctx = httpx.WithOrgID(ctx, "org-1")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	sk.handlePromptImportFromDisk(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /api/prompts/from-disk in multi mode = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, httpx.ReasonNotFound, "")
}
