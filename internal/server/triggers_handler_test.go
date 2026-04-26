package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedPrompt creates a user prompt so triggers have something to reference.
func seedPrompt(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, "POST", "/api/prompts", map[string]any{
		"name": "Test prompt",
		"body": "Do something",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed prompt: %d: %s", rec.Code, rec.Body.String())
	}
	var p domain.Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	return p.ID
}

func TestTriggerCreate_WithAutonomySuitability(t *testing.T) {
	s := newTestServer(t)
	promptID := seedPrompt(t, s)

	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":                promptID,
		"event_type":               "github:pr:ci_check_failed",
		"min_autonomy_suitability": 0.7,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var trigger domain.PromptTrigger
	if err := json.Unmarshal(rec.Body.Bytes(), &trigger); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if trigger.MinAutonomySuitability != 0.7 {
		t.Errorf("expected 0.7, got %v", trigger.MinAutonomySuitability)
	}
}

func TestTriggerCreate_DefaultAutonomySuitability(t *testing.T) {
	s := newTestServer(t)
	promptID := seedPrompt(t, s)

	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":  promptID,
		"event_type": "github:pr:ci_check_failed",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var trigger domain.PromptTrigger
	if err := json.Unmarshal(rec.Body.Bytes(), &trigger); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if trigger.MinAutonomySuitability != 0.0 {
		t.Errorf("expected default 0.0, got %v", trigger.MinAutonomySuitability)
	}
}

func TestTriggerCreate_AutonomySuitabilityTooHigh(t *testing.T) {
	s := newTestServer(t)
	promptID := seedPrompt(t, s)

	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":                promptID,
		"event_type":               "github:pr:ci_check_failed",
		"min_autonomy_suitability": 1.5,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestTriggerCreate_AutonomySuitabilityNegative(t *testing.T) {
	s := newTestServer(t)
	promptID := seedPrompt(t, s)

	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":                promptID,
		"event_type":               "github:pr:ci_check_failed",
		"min_autonomy_suitability": -0.1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestTriggerUpdate_WithAutonomySuitability(t *testing.T) {
	s := newTestServer(t)
	promptID := seedPrompt(t, s)

	// Create trigger with default autonomy
	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":  promptID,
		"event_type": "github:pr:ci_check_failed",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &created)

	// Update with autonomy threshold
	rec = doJSON(t, s, "PUT", "/api/triggers/"+created.ID, map[string]any{
		"scope_predicate_json":     "",
		"breaker_threshold":        4,
		"min_autonomy_suitability": 0.5,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d: %s", rec.Code, rec.Body.String())
	}

	var updated domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.MinAutonomySuitability != 0.5 {
		t.Errorf("expected 0.5, got %v", updated.MinAutonomySuitability)
	}

	// Verify persisted via list
	rec = doJSON(t, s, "GET", "/api/triggers", nil)
	var triggers []domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &triggers)
	for _, tr := range triggers {
		if tr.ID == created.ID {
			if tr.MinAutonomySuitability != 0.5 {
				t.Errorf("persisted value mismatch: %v", tr.MinAutonomySuitability)
			}
			return
		}
	}
	t.Fatal("trigger not found in list")
}

func TestTriggerUpdate_AutonomySuitabilityOutOfRange(t *testing.T) {
	s := newTestServer(t)
	promptID := seedPrompt(t, s)

	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":  promptID,
		"event_type": "github:pr:ci_check_failed",
	})
	var created domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &created)

	rec = doJSON(t, s, "PUT", "/api/triggers/"+created.ID, map[string]any{
		"scope_predicate_json":     "",
		"breaker_threshold":        4,
		"min_autonomy_suitability": 2.0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// TestTriggerRoundTrip_AutonomySuitability verifies that the value survives
// a full POST → GET → PUT → GET cycle without drift.
func TestTriggerRoundTrip_AutonomySuitability(t *testing.T) {
	s := newTestServer(t)
	if err := db.SeedTaskRules(s.db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	promptID := seedPrompt(t, s)

	// Create with threshold
	rec := doJSON(t, s, "POST", "/api/triggers", map[string]any{
		"prompt_id":                promptID,
		"event_type":               "github:pr:review_requested",
		"min_autonomy_suitability": 0.65,
		"breaker_threshold":        5,
		"cooldown_seconds":         120,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &created)

	// Read back via list
	rec = doJSON(t, s, "GET", "/api/triggers", nil)
	var all []domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &all)

	var found *domain.PromptTrigger
	for i := range all {
		if all[i].ID == created.ID {
			found = &all[i]
			break
		}
	}
	if found == nil {
		t.Fatal("created trigger not in list")
	}
	if found.MinAutonomySuitability != 0.65 {
		t.Errorf("GET returned %v, want 0.65", found.MinAutonomySuitability)
	}
	if found.BreakerThreshold != 5 {
		t.Errorf("breaker: %d, want 5", found.BreakerThreshold)
	}

	// Update: change autonomy, keep others
	rec = doJSON(t, s, "PUT", "/api/triggers/"+created.ID, map[string]any{
		"scope_predicate_json":     "",
		"breaker_threshold":        5,
		"min_autonomy_suitability": 0.3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d: %s", rec.Code, rec.Body.String())
	}

	var updated domain.PromptTrigger
	json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.MinAutonomySuitability != 0.3 {
		t.Errorf("PUT returned %v, want 0.3", updated.MinAutonomySuitability)
	}
}
