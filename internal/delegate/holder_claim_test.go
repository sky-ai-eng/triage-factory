package delegate

import (
	"context"
	"testing"
)

// holderClaimFor mints a live claim on the conversation the way a go-live
// confirmation does, and returns its id, for a fixture that drives a holder's
// fenced write without dispatching a real engagement.
func holderClaimFor(t *testing.T, s *Spawner, orgID, conversationID string) string {
	t.Helper()
	c, err := s.conversations.SetExecutorSystem(context.Background(), orgID, conversationID, "exec-holder-test", 1)
	if err != nil || c == nil {
		t.Fatalf("mint a holder claim: claim=%v err=%v", c, err)
	}
	return c.ID
}
