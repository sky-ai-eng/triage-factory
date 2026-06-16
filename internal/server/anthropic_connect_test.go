package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// anthropicModelsStub stands up a host whose every response carries the given
// status, standing in for Anthropic's GET /v1/models key check. The connect
// handler's validator (auth.ValidateAnthropicAPIKey) is pointed at it via
// auth.SetAnthropicModelsURLForTest.
func anthropicModelsStub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// orgHasAnthropicKey reads the GET /api/settings/org presence flag — the same
// has_anthropic_api_key signal the "Configured" UI reads.
func orgHasAnthropicKey(t *testing.T, s *Server) bool {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/settings/org", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		HasAnthropicAPIKey bool `json:"has_anthropic_api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.HasAnthropicAPIKey
}

// TestAnthropicConnect_ValidKey_StoresAndSetsRef drives the BYOK happy path: a
// key that validates (stub 200) is stored in the vault under anthropic_api_key,
// AnthropicAPIKeyRef is set (so has_anthropic_api_key flips true), and the
// resolver would find it.
func TestAnthropicConnect_ValidKey_StoresAndSetsRef(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)

	rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "  sk-ant-valid  "})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Stored under the canonical key — trimmed of the surrounding whitespace.
	stored, err := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get anthropic_api_key: %v", err)
	}
	if stored != "sk-ant-valid" {
		t.Errorf("stored key = %q, want the trimmed key", stored)
	}
	if !orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=false after storing a valid key")
	}
}

// TestAnthropicConnect_BadKey_Returns422_NothingStored pins the rejection path:
// a key the host rejects (stub 401) is a 422, the message is surfaced, and no
// secret nor ref is written.
func TestAnthropicConnect_BadKey_Returns422_NothingStored(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusUnauthorized).URL)

	rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "sk-ant-bad"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error == "" {
		t.Error("422 carried no error message")
	}

	stored, _ := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("key stored despite rejection: %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after a rejected key")
	}
}

// TestAnthropicConnect_EmptyKey_ClearsStoredKey drives the "use system
// credentials" path: an empty key clears any stored key and its ref, without
// touching the validator (so it works with no stub). It first stores a key,
// then clears it.
func TestAnthropicConnect_EmptyKey_ClearsStoredKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	// Seed a stored key via the validated path (stub 200).
	auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
	if rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "sk-ant-seed"}); rec.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !orgHasAnthropicKey(t, s) {
		t.Fatal("seed did not set has_anthropic_api_key")
	}

	// Empty key clears it — no validator round-trip.
	rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "   "})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	stored, _ := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("key still stored after clear: %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after clearing")
	}
}

// TestAnthropicConnect_Unreachable_Returns422 pins that a transport failure
// (stub closed) surfaces as a 422 on the connect surface (mirroring
// handleJiraConnect's single-422 shape) and stores nothing.
func TestAnthropicConnect_Unreachable_Returns422(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	// Stand a stub up to claim a port, then close it so dials are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	auth.SetAnthropicModelsURLForTest(t, deadURL)

	rec := doJSON(t, s, "POST", "/api/anthropic/connect", map[string]any{"api_key": "sk-ant-x"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
}

// TestAnthropicConnect_BulkPostNoBackDoor pins that the bulk org-settings POST
// is NOT a write path for the key: a non-empty anthropic_api_key in that body
// is ignored (the field was removed), so the only way to set it is the
// validated endpoint.
func TestAnthropicConnect_BulkPostNoBackDoor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	rec := doJSON(t, s, "POST", "/api/settings/org", map[string]any{
		"github_base_url":      "https://github.com",
		"anthropic_api_key":    "sk-ant-sneaky",
		"max_llm_model_tier":   "",
		"github_poll_interval": "5m0s",
		"jira_poll_interval":   "5m0s",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk settings status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	stored, _ := s.secrets.Get(ctx, runmode.LocalDefaultOrgID, "anthropic_api_key")
	if stored != "" {
		t.Errorf("bulk POST stored the key (back door): %q", stored)
	}
	if orgHasAnthropicKey(t, s) {
		t.Error("has_anthropic_api_key=true after a bulk POST (back door)")
	}
}
