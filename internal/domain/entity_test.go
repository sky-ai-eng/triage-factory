package domain

import "testing"

// TestSlackSourceID pins the Slack entity source_id convention TFAC-510's
// thread-create consumes: "<channel_id>/<thread_ts>", with the caller passing
// the message's own ts as threadTS for a mention on a root channel message.
func TestSlackSourceID(t *testing.T) {
	if got := SlackSourceID("C0125", "1700000000.000100"); got != "C0125/1700000000.000100" {
		t.Errorf("threaded mention: got %q, want C0125/1700000000.000100", got)
	}
	// Root-message mention: the caller passes the message ts as the thread root,
	// so the format is identical — the helper doesn't special-case it.
	if got := SlackSourceID("C0125", "1699999999.000001"); got != "C0125/1699999999.000001" {
		t.Errorf("root mention: got %q, want C0125/1699999999.000001", got)
	}
}
