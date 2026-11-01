package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// TestMultiModeGates_Return404: in multi mode the prompt import from disk, which
// walks this process's ~/.claude/skills, answers 404 like a route that does not
// exist, rather than scanning shared infrastructure's $HOME.
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
	httpxtest.AssertFirstError(t, rec, httpx.ReasonNotFound, "")
}
