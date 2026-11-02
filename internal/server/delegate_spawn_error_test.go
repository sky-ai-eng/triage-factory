package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestWriteDelegateSpawnError_TeamArchived: a delegation refused because the
// task's team is archived is a 409 carrying SPAWN_FAILED, like the busy-task
// refusal beside it, and not the 500 an unrecognized spawn failure gets.
func TestWriteDelegateSpawnError_TeamArchived(t *testing.T) {
	rec := httptest.NewRecorder()
	writeDelegateSpawnError(rec, fmt.Errorf("fire: %w", delegate.ErrTeamArchived))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonSpawnFailed || body.Errors[0].Field != "" {
		t.Errorf("errors = %+v, want one SPAWN_FAILED with no field", body.Errors)
	}
}
