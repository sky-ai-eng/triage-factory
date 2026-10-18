package slack

import (
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// Slack publishes its Web API limits per method, as calls one app may make to
// the method in one workspace in a minute, and states nothing about them on a
// response: its only signal is a 429 with Retry-After. So the client counts
// its own calls to each method over the last minute, per org and credential,
// and reports the count against the method's published limit as what is left
// of it (countCall). The count covers only this process's calls: in multi,
// the calls a delegated run makes through its sidecar spend the same budget
// and are counted only in that sidecar, so a pod's count is a floor.

// slackWindow is the span Slack states its limits over.
const slackWindow = time.Minute

// The per-minute floor of each of Slack's tiers TF calls a method in: a Tier 3
// method allows "50+ per minute", and Slack tolerates bursts above that.
const (
	slackTier2 = 20
	slackTier3 = 50
	slackTier4 = 100
)

// slackMethodLimits is the published per-minute limit, per app per workspace,
// of every Web API method TF calls: the floor of its tier, or the figure Slack
// publishes for the method itself. A method Slack gives no such figure maps to
// 0 and is reported, like a method missing from the table, with its count and
// no limit:
//   - auth.test and chat.getPermalink are Special, "hundreds of requests per
//     minute";
//   - chat.postMessage is Special and metered per channel, about one message a
//     second, which a count per method cannot show.
//
// conversations.history and conversations.replies are Tier 3 for an app a
// workspace builds for itself, which is how each org creates TF's from its
// manifest. Slack limits an app distributed outside its Marketplace to one
// call a minute to each.
var slackMethodLimits = map[string]int{
	"apps.connections.open":        slackTier3,
	"assistant.threads.setStatus":  600, // Special: 600 a minute
	"auth.test":                    0,
	"bots.info":                    slackTier3,
	"chat.getPermalink":            0,
	"chat.postMessage":             0,
	"chat.update":                  slackTier3,
	"conversations.history":        slackTier3,
	"conversations.info":           slackTier3,
	"conversations.join":           slackTier3,
	"conversations.list":           slackTier2,
	"conversations.replies":        slackTier3,
	"files.completeUploadExternal": slackTier4,
	"files.getUploadURLExternal":   slackTier4,
	"files.info":                   slackTier4,
	"reactions.add":                slackTier3,
	"users.conversations":          slackTier3,
	"users.info":                   slackTier4,
}

// countCall counts a call c made to method that Slack answered, refused or
// not, and reports the method's budget for c's credential as of it: the calls
// counted in the last slackWindow, what is left of the method's published
// limit, and a reset slackWindow away, when every call counted has left the
// window. A refusal is also counted as the method's (upstream.RecordRefused).
// A caller that names no org or no credential counts nothing.
func (c slackCaller) countCall(method string, refused bool) {
	if c.orgID == "" || c.credential == "" {
		return
	}
	now := time.Now()
	used := slackCalls.add(slackCallKey{c.orgID, c.credential, method}, now)
	b := upstream.Budget{Used: used, Reset: now.Add(slackWindow)}
	if limit := slackMethodLimits[method]; limit > 0 {
		b.Limit, b.Remaining = limit, max(limit-used, 0)
	}
	upstream.RecordBudget(upstream.Slack, c.orgID, c.credential, method, b)
	if refused {
		upstream.RecordRefused(upstream.Slack, c.orgID, c.credential, method)
	}
}

// slackCalls is the process-wide count countCall keeps: in memory, so a
// restart starts every method's count over.
var slackCalls = newSlackCallWindow()

type slackCallKey struct{ orgID, credential, method string }

// slackCallWindow holds the times of the calls made under each key in the
// last slackWindow, oldest first.
type slackCallWindow struct {
	mu    sync.Mutex
	byKey map[slackCallKey][]time.Time
	// swept is when add last dropped the keys with no call in the window.
	swept time.Time
}

func newSlackCallWindow() *slackCallWindow {
	return &slackCallWindow{byKey: map[slackCallKey][]time.Time{}}
}

// add counts a call under k at now and returns how many of k's calls fall in
// the window ending at now, this one included.
func (w *slackCallWindow) add(k slackCallKey, now time.Time) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	start := now.Add(-slackWindow)
	if now.Sub(w.swept) >= slackWindow {
		for key, calls := range w.byKey {
			if !calls[len(calls)-1].After(start) {
				delete(w.byKey, key)
			}
		}
		w.swept = now
	}
	calls := w.byKey[k]
	i := 0
	for i < len(calls) && !calls[i].After(start) {
		i++
	}
	calls = append(calls[i:], now)
	w.byKey[k] = calls
	return len(calls)
}
