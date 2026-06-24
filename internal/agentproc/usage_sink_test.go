package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func intp(i int) *int { return &i }

// TestUsageSink_SumsAcrossMessages pins the core accumulation: each
// OnMessage adds its per-field token counts, so the sink ends a Run with
// the totals across every assistant message.
func TestUsageSink_SumsAcrossMessages(t *testing.T) {
	s := &UsageSink{}

	if err := s.OnMessage(&domain.AgentMessage{
		InputTokens:         intp(10),
		OutputTokens:        intp(2),
		CacheReadTokens:     intp(100),
		CacheCreationTokens: intp(5),
	}); err != nil {
		t.Fatalf("OnMessage: %v", err)
	}
	if err := s.OnMessage(&domain.AgentMessage{
		InputTokens:         intp(7),
		OutputTokens:        intp(3),
		CacheReadTokens:     intp(50),
		CacheCreationTokens: intp(1),
	}); err != nil {
		t.Fatalf("OnMessage: %v", err)
	}

	if s.InputTokens != 17 {
		t.Errorf("InputTokens = %d, want 17", s.InputTokens)
	}
	if s.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", s.OutputTokens)
	}
	if s.CacheReadTokens != 150 {
		t.Errorf("CacheReadTokens = %d, want 150", s.CacheReadTokens)
	}
	if s.CacheCreationTokens != 6 {
		t.Errorf("CacheCreationTokens = %d, want 6", s.CacheCreationTokens)
	}
}

// TestUsageSink_HandlesNilPointers covers the common case: a tool-result
// message carries no usage block, so the per-field pointers are nil. A nil
// field must contribute zero, not panic, and must not disturb other fields.
func TestUsageSink_HandlesNilPointers(t *testing.T) {
	s := &UsageSink{}

	// A message with only some fields populated.
	if err := s.OnMessage(&domain.AgentMessage{InputTokens: intp(4)}); err != nil {
		t.Fatalf("OnMessage: %v", err)
	}
	// A message with all-nil usage (the tool-result shape).
	if err := s.OnMessage(&domain.AgentMessage{}); err != nil {
		t.Fatalf("OnMessage all-nil: %v", err)
	}
	// A nil message must be ignored entirely.
	if err := s.OnMessage(nil); err != nil {
		t.Fatalf("OnMessage(nil): %v", err)
	}

	if s.InputTokens != 4 {
		t.Errorf("InputTokens = %d, want 4", s.InputTokens)
	}
	if s.OutputTokens != 0 || s.CacheReadTokens != 0 || s.CacheCreationTokens != 0 {
		t.Errorf("unexpected non-zero token field: %+v", s)
	}
}

// TestUsageSink_OnSessionIgnored confirms OnSession is a no-op that never
// errors — the sink only cares about per-message usage.
func TestUsageSink_OnSessionIgnored(t *testing.T) {
	s := &UsageSink{}
	if err := s.OnSession("session-123"); err != nil {
		t.Fatalf("OnSession: %v", err)
	}
	if (*s != UsageSink{}) {
		t.Errorf("OnSession mutated the sink: %+v", s)
	}
}
