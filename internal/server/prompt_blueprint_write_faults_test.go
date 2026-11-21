package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestPromptAndBlueprintWrites_ReportEveryFault: a prompt or blueprint write
// whose body has several faults answers all of them in one 400, rather than
// the first, so a caller fixes the body in one round trip.
func TestPromptAndBlueprintWrites_ReportEveryFault(t *testing.T) {
	s := newTestServer(t)
	promptRec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{"name": "existing", "body": "b"})
	if promptRec.Code != http.StatusCreated {
		t.Fatalf("seed prompt: %d (body: %s)", promptRec.Code, promptRec.Body.String())
	}
	var seeded domain.Prompt
	if err := json.Unmarshal(promptRec.Body.Bytes(), &seeded); err != nil {
		t.Fatalf("decode seeded prompt: %v", err)
	}
	promptID := seeded.ID
	chain, _ := createChainBlueprint(t, s, "Chain", 2)

	type fault struct{ reason, field string }
	for _, tc := range []struct {
		name, method, path string
		body               map[string]any
		want               []fault
	}{
		{"prompt create", http.MethodPost, "/api/prompts",
			map[string]any{"name": "", "body": "", "model": "not-a-model"},
			[]fault{{httpx.ReasonMissingField, "name"}, {httpx.ReasonMissingField, "body"}, {httpx.ReasonInvalidField, "model"}}},
		{"prompt replace", http.MethodPut, "/api/prompts/" + promptID,
			map[string]any{"name": "", "body": "", "model": "not-a-model"},
			[]fault{{httpx.ReasonMissingField, "name"}, {httpx.ReasonMissingField, "body"}, {httpx.ReasonInvalidField, "model"}}},
		{"prompt upload", http.MethodPost, "/api/prompts/upload",
			map[string]any{"content": ""},
			[]fault{{httpx.ReasonMissingField, "content"}, {httpx.ReasonMissingField, "name"}}},
		{"blueprint create with first_prompt", http.MethodPost, "/api/blueprints",
			map[string]any{"first_prompt": map[string]any{"name": "", "body": "", "model": "not-a-model"}},
			[]fault{{httpx.ReasonMissingField, "first_prompt.name"}, {httpx.ReasonMissingField, "first_prompt.body"}, {httpx.ReasonInvalidField, "first_prompt.model"}}},
		{"blueprint reconnect", http.MethodPost, "/api/blueprints/" + chain + "/reconnect",
			map[string]any{},
			[]fault{{httpx.ReasonMissingField, "at_step_index"}, {httpx.ReasonMissingField, "target_blueprint_id"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			var got []fault
			for _, item := range decodeErrorItems(t, rec) {
				got = append(got, fault{item.Reason, item.Field})
			}
			if len(got) != len(tc.want) {
				t.Fatalf("errors = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("errors = %+v, want %+v", got, tc.want)
					break
				}
			}
		})
	}
}
