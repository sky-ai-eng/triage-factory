package agentloop

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRun_PersonMessageSettings is how a person's message moves the model and
// effort an engagement calls with: within the engagement's provider from the
// next call on, and outside it by ending the engagement before any call.
func TestRun_PersonMessageSettings(t *testing.T) {
	const (
		start = "claude-sonnet-5-5"
		other = "claude-fable-5-1"
		far   = "us.anthropic.claude-sonnet-5-5"
	)
	person := func(content, model, effort string, delivered bool) *domain.Message {
		return &domain.Message{ConversationID: "conv", UserID: "u", Role: "user", Content: content,
			Model: model, Effort: effort, Delivered: boolPtr(delivered)}
	}
	params := func() Params {
		p := testParams()
		p.Model = start
		p.SwitchableModels = []string{start, other}
		return p
	}

	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a steer on a switchable model changes the next request", func(t *testing.T) {
			tr := newMemTranscript(*person("go", start, "", false))
			p := &scriptedProvider{turns: []scriptedTurn{
				{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}, onCall: func() {
					_, _ = tr.Insert(context.Background(), "org", person("think harder", other, domain.EffortHigh, false))
				}},
				{text: "done"},
			}}
			if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), params()); got.Kind != ResultConcluded {
				t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
			}
			if len(p.requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(p.requests))
			}
			if r := p.requests[0]; r.Model != start || r.Effort != "" {
				t.Errorf("first request = (%q, %q), want (%s, default)", r.Model, r.Effort, start)
			}
			if r := p.requests[1]; r.Model != other || r.Effort != domain.EffortHigh {
				t.Errorf("second request = (%q, %q), want (%s, high)", r.Model, r.Effort, other)
			}
			reply := tr.find(func(m domain.Message) bool { return m.Role == "assistant" && m.Content == "done" })
			if reply == nil || reply.Model != other {
				t.Errorf("the reply row records model %v, want %s", reply, other)
			}
		}},
		{"a model outside the set ends the engagement before any call", func(t *testing.T) {
			tr := newMemTranscript(*person("switch providers", far, domain.EffortLow, false))
			p := &scriptedProvider{}
			got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), params())
			if got.Kind != ResultProviderSwitch || got.Model != far {
				t.Fatalf("result = (%v, %q), want a provider switch to %s (err: %v)", got.Kind, got.Model, far, got.Err)
			}
			if p.calls != 0 {
				t.Errorf("provider calls = %d, want none", p.calls)
			}
		}},
		{"a compaction that drops the adopting row keeps its settings", func(t *testing.T) {
			tr := newMemTranscript(*person("earlier", start, domain.EffortLow, true), *person("later", other, domain.EffortHigh, false))
			p := &scriptedProvider{turns: []scriptedTurn{
				{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}, onCall: func() {
					tr.mu.Lock()
					defer tr.mu.Unlock()
					for i := range tr.rows {
						if tr.rows[i].Content == "later" {
							tr.rows[i].WindowState = domain.MessageWindowInactive
						}
					}
				}},
				{text: "done"},
			}}
			if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), params()); got.Kind != ResultConcluded {
				t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
			}
			for i, r := range p.requests {
				if r.Model != other || r.Effort != domain.EffortHigh {
					t.Errorf("request %d = (%q, %q), want the adopted (%s, high)", i, r.Model, r.Effort, other)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}
