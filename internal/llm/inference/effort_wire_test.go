package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestClient_EffortReachesAnthropicAsOutputConfigEffort pins what bifrost
// sends for each of TF's five effort levels on the 5.x models: the level
// itself, as output_config.effort, on every model CarriesEffort answers true
// for, and no output_config.effort on any other. The catalog offers a native
// model the levels CarriesEffort allows, so this is the test that the offered
// levels and the wire agree; a bifrost upgrade that changes either fails here.
//
// The pinned bifrost carries an effort to Sonnet 5.x and Fable 5.x, and sends
// Opus 5.x and Haiku 5.5 a thinking budget instead. The first half is required
// below so the test cannot pass by carrying nothing.
func TestClient_EffortReachesAnthropicAsOutputConfigEffort(t *testing.T) {
	models := []string{
		"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-fable-5-1",
		"claude-sonnet-5", "claude-opus-5", "claude-fable-5",
	}
	mustCarry := map[string]bool{"claude-sonnet-5-5": true, "claude-fable-5-1": true, "claude-sonnet-5": true, "claude-fable-5": true}

	var body []byte
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range anthropicStreamFixture("m") {
			fmt.Fprint(w, frame)
			w.(http.Flusher).Flush()
		}
	}))
	defer provider.Close()

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "sk-ant-test",
		BaseURL:  provider.URL,
		Models:   models,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, model := range models {
		for _, level := range domain.EffortLevels {
			t.Run(model+"/"+level, func(t *testing.T) {
				body = nil
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := client.Stream(ctx, Request{
					Provider: ProviderAnthropic,
					Model:    model,
					Rows:     []domain.Message{{Role: "user", Content: "hi"}},
					Effort:   level,
				}); err != nil {
					t.Fatalf("stream: %v", err)
				}
				var sent struct {
					OutputConfig *struct {
						Effort *string `json:"effort"`
					} `json:"output_config"`
				}
				if err := json.Unmarshal(body, &sent); err != nil {
					t.Fatalf("decode the request body: %v (%s)", err, body)
				}
				var got string
				if sent.OutputConfig != nil && sent.OutputConfig.Effort != nil {
					got = *sent.OutputConfig.Effort
				}
				carries := CarriesEffort(model, level)
				if mustCarry[model] && !carries {
					t.Fatalf("CarriesEffort(%s, %s) = false, want true", model, level)
				}
				if carries && got != level {
					t.Fatalf("output_config.effort = %q, want %q (body: %s)", got, level, body)
				}
				if !carries && got != "" {
					t.Fatalf("output_config.effort = %q on a model CarriesEffort refuses (body: %s)", got, body)
				}
			})
		}
	}
}
