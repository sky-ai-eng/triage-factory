// Slack lifecycle adapters (TFAC-597): everything the bot does in Slack
// without the agent asking for it. A single lifecycleAdapter, started via
// api.OnReady (the socket manager's precedent — api.Bus() is nil during
// install(), see install.go's package doc), subscribes to the conversation-lifecycle
// and routing-disposition sentinels the blocking core tickets publish
// (system:conversation:status, system:conversation:activity, system:routing:disposition) and
// drives four surfaces purely off them:
//
//   - a 👀 reaction acknowledging an explicit @-mention (the ONLY reaction —
//     no ⏳/✅/❌/⚠️ lifecycle, ratified out; an un-mentioned follow-up in a
//     thread the bot already owns gets none),
//   - the assistant.threads.setStatus working indicator — both the
//     "<app name> is …" status line and, via loading_messages, the animated
//     in-conversation loading bubble (left unset, Slack rotates its own
//     stock phrases there) — kept live by real conversation-status/activity traffic
//     (no streaming),
//   - a brief thread reply when a conversation fails,
//   - a one-line "connected but not configured" reply when a mention
//     matches no handler/owner.
//
// The additive flip that makes a re-mention on a live conversation inject instead of
// queueing a second conversation lives in events.go (one field on the registered
// schema); this file only consumes the bus, it never touches core sentinels
// or the inject branch itself.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// slackLifecycleNoMatchCopy is the taskless_no_handler / taskless_no_owner
// thread reply — plain text, no emoji (ratified: no automatic content
// messages except this one and the failure note).
const slackLifecycleNoMatchCopy = "I'm connected, but no team is set up to handle mentions in this channel yet. A team admin can track this channel under Team Settings → Slack."

// slackLifecycleFailureCopy is the brief failure-note prefix; postFailureReply
// appends a "Details: <url>" fragment when api.PublicURL() is non-empty.
const slackLifecycleFailureCopy = "Something went wrong and this run failed."

// slackLifecycleInitialStatusText is the setStatus text a per-conversation worker
// starts with, before any tool-activity sentinel has arrived to refine it.
const slackLifecycleInitialStatusText = "is working on it…"

// slackLifecycleInitialLoadingText is slackLifecycleInitialStatusText's
// loading-bubble counterpart. The bubble renders bare — no app-name prefix —
// so it needs standalone phrasing.
const slackLifecycleInitialLoadingText = "Working on it…"

// slackLifecycleAckReaction is the ONLY reaction this adapter ever adds —
// ratified: no ⏳/✅/❌/⚠️ lifecycle.
const slackLifecycleAckReaction = "eyes"

// Tunable timings, as package vars (not consts) so tests can shrink them —
// mirrors socket.go's socketReconcileInterval convention.
var (
	// slackLifecycleStatusRefreshInterval re-sends setStatus periodically so
	// the platform's 2-minute auto-expiry (the dead-man switch: a TF crash
	// makes the indicator vanish within 2 minutes, never lingers longer)
	// never fires while a conversation is genuinely still going.
	slackLifecycleStatusRefreshInterval = 45 * time.Second
	// slackLifecycleActivityDebounce bounds setStatus calls driven by
	// system:conversation:activity to at most one per this interval per conversation — activity
	// can arrive far faster than it should render.
	slackLifecycleActivityDebounce = 3 * time.Second
	// slackLifecycleIdleTimeout reaps a worker whose conversation's terminal sentinel
	// was dropped (the bus is at-most-once): no status/activity update for
	// this long clears the indicator and stops the worker. The platform's own
	// 2-minute expiry already bounds the user-visible lag to 2 minutes past
	// whatever this timeout adds.
	slackLifecycleIdleTimeout = 30 * time.Minute
	// slackLifecycleNow is time.Now, overridable in tests that need to force
	// cache-entry expiry without a real wait.
	slackLifecycleNow = time.Now
)

// slackLifecycleEventBuffer bounds the adapter's internal dispatch channel —
// Handle (the bus callback) pushes onto it non-blocking, per the eventbus
// contract that subscriber handlers must return fast.
const slackLifecycleEventBuffer = 256

// slackLifecycleCacheMaxEntries bounds the per-conversation verdict cache (conversationEntry)
// so a long-lived process doesn't grow it unboundedly across every conversation it
// has ever seen. slackLifecycleCachePruneTarget is where a prune pass brings
// the cache back down to — deliberately below the cap, not equal to it, so
// pruning doesn't re-trigger on the very next insert: the O(n log n) sort in
// pruneConversationCache is amortized over the (max-target) inserts between prune
// passes instead of paid on every single insert once the cache is full.
var (
	slackLifecycleCacheMaxEntries  = 2000
	slackLifecycleCachePruneTarget = 1800
)

// lifecycleAdapter is the TFAC-597 background worker: constructed at
// install() with everything it needs (stores, HTTP client, the PublicURL/Bus
// read-through accessors), started via api.OnReady(adapter.run) once app
// wiring completes.
type lifecycleAdapter struct {
	stores    db.Stores
	client    *http.Client
	publicURL func() string
	bus       func() *eventbus.Bus
}

// newLifecycleAdapter constructs the adapter. Called from install(); run
// itself does not fire until api.OnReady's hook does.
func newLifecycleAdapter(stores db.Stores, client *http.Client, publicURL func() string, bus func() *eventbus.Bus) *lifecycleAdapter {
	return &lifecycleAdapter{stores: stores, client: client, publicURL: publicURL, bus: bus}
}

// run is the function api.OnReady hands to StartExtensionWorkers. It
// subscribes to the bus (safe here — OnReady hooks fire post-wiring, unlike
// install() itself), then single-threadedly dispatches every sentinel it
// sees to the disposition/conversation-status/conversation-activity handlers below. The bus
// Handle callback only decodes far enough to route (it never touches a
// store or Slack) and pushes onto an internal channel — the actual work runs
// on this goroutine (plus the per-conversation workers it starts), never on the bus's
// own per-subscriber drain goroutine.
func (a *lifecycleAdapter) run(ctx context.Context) {
	bus := a.bus()
	if bus == nil {
		return
	}

	ch := make(chan domain.Event, slackLifecycleEventBuffer)
	unsubscribe := bus.Subscribe(eventbus.Subscriber{
		Name:   "slack-lifecycle",
		Filter: []string{"system:conversation:", "system:routing:"},
		Handle: func(evt domain.Event) {
			select {
			case ch <- evt:
			default:
				slackLog.Warn("slack lifecycle: dispatch backlog full, dropping event", "event_type", evt.EventType)
			}
		},
	})

	conversations := map[string]*conversationEntry{}
	for {
		select {
		case <-ctx.Done():
			unsubscribe()
			drainLifecycleWorkers(conversations)
			return
		case evt := <-ch:
			a.dispatch(ctx, evt, conversations)
		}
	}
}

// drainLifecycleWorkers waits for every still-live per-conversation worker to notice
// ctx cancellation and finish its own cleanup (each worker watches the same
// ctx directly and clears its status on the way out — see conversationStatusWorker.loop)
// before run returns, mirroring the socket manager's stopAll join discipline.
// Joins prevDone too: a worker retired by a terminal sentinel (stop is
// fire-and-forget) may still be mid-teardown at shutdown with no live
// successor to imply it finished; a long-closed prevDone costs nothing.
func drainLifecycleWorkers(conversations map[string]*conversationEntry) {
	var wg sync.WaitGroup
	join := func(done chan struct{}) {
		if done == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-done
		}()
	}
	for _, entry := range conversations {
		if entry.worker != nil {
			join(entry.worker.done)
		}
		join(entry.prevDone)
	}
	wg.Wait()
}

// dispatch routes one bus sentinel to its handler by type. Anything else
// matching the "system:conversation:"/"system:routing:" filter that isn't one of
// these three exact types (there is none today) is silently ignored.
func (a *lifecycleAdapter) dispatch(ctx context.Context, evt domain.Event, conversations map[string]*conversationEntry) {
	switch evt.EventType {
	case domain.EventSystemRoutingDisposition:
		a.handleDisposition(ctx, evt)
	case domain.EventSystemConversationStatus:
		a.handleConversationStatus(ctx, evt, conversations)
	case domain.EventSystemConversationActivity:
		a.handleConversationActivity(ctx, evt, conversations)
	}
}

// --- disposition consumer: 👀 acknowledge + no-match reply ---

// handleDisposition reacts to system:routing:disposition for slack:message
// events only, and every reactive surface here fires ONLY for a message that
// explicitly @-mentioned the bot. task_created/task_bumped add the single 👀
// reaction (acknowledgeMention); taskless_no_handler/taskless_no_owner post
// the one-line not-configured reply (replyNotConfigured). Both load the
// message metadata, gate on its explicit-mention flag, and only then resolve
// the bot token: an un-mentioned follow-up in a thread the bot already owns is
// handled silently — a per-message reaction or reply on top of the task/conversation it
// already drives (whether that absorbs into an in-flight conversation or starts a fresh
// one) is just noise. frozen/taskless_unroutable/error are inert by design.
// Every step here is best-effort: log-and-drop on any store/API failure, never
// a retry loop.
func (a *lifecycleAdapter) handleDisposition(ctx context.Context, evt domain.Event) {
	var disp events.SystemRoutingDispositionMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &disp); err != nil {
		slackLog.Warn("slack lifecycle: decode routing disposition failed", "error", err)
		return
	}
	if disp.EventType != domain.EventSlackMessage {
		return
	}

	switch disp.Disposition {
	case events.DispositionTaskCreated, events.DispositionTaskBumped:
		a.acknowledgeMention(ctx, evt.OrgID, disp.EventID)
	case events.DispositionTasklessNoHandler, events.DispositionTasklessNoOwner:
		a.replyNotConfigured(ctx, evt.OrgID, disp.EventID)
	}
}

// acknowledgeMention adds the 👀 reaction to the mention message itself
// (meta.TS, never the thread root) — but only when that message explicitly
// @-mentioned the bot. An un-mentioned follow-up in a thread the bot already
// owns (Mentioned=false) gets no reaction: stamping 👀 on every message in an
// engaged thread is noise, and any conversation the follow-up drives already shows a
// working indicator that signals it landed.
func (a *lifecycleAdapter) acknowledgeMention(ctx context.Context, orgID, eventID string) {
	meta, ok := a.messageMetadata(ctx, orgID, eventID)
	if !ok || !meta.Mentioned {
		return
	}
	token, ok := a.resolveBotToken(ctx, orgID, meta.WorkspaceID, meta.APIAppID)
	if !ok {
		return
	}
	if err := slackReactionsAdd(ctx, a.client, token, meta.Channel, meta.TS, slackLifecycleAckReaction); err != nil {
		slackLog.Warn("slack lifecycle: reactions.add failed", "error", err)
	}
}

// replyNotConfigured posts the taskless one-liner as a thread reply, rooted
// at ThreadTS when the mention was already inside a thread, else the
// mention's own ts (a root-message mention starts its own thread). Like
// acknowledgeMention, it fires only for an explicit @-mention — an
// un-mentioned engaged-thread follow-up (Mentioned=false) never draws an
// automated reply, so a config-changed thread the bot already owns doesn't
// suddenly answer a plain follow-up with "not configured".
func (a *lifecycleAdapter) replyNotConfigured(ctx context.Context, orgID, eventID string) {
	meta, ok := a.messageMetadata(ctx, orgID, eventID)
	if !ok || !meta.Mentioned {
		return
	}
	token, ok := a.resolveBotToken(ctx, orgID, meta.WorkspaceID, meta.APIAppID)
	if !ok {
		return
	}
	threadTS := meta.ThreadTS
	if threadTS == "" {
		threadTS = meta.TS
	}
	if _, err := slackChatPostMessage(ctx, a.client, token, slackMessageParams{
		Channel: meta.Channel, ThreadTS: threadTS, Text: slackLifecycleNoMatchCopy,
	}); err != nil {
		slackLog.Warn("slack lifecycle: not-configured reply failed", "error", err)
	}
}

// messageMetadata loads and parses eventID's SlackMessageMetadata. ok=false
// covers a missing/empty/unparsable row alike — a best-effort "can't
// proceed", logged and dropped. Resolving the bot token is a deliberately
// SEPARATE step (resolveBotToken) rather than folded in here: both callers
// gate on meta.Mentioned first, so an un-mentioned engaged-thread follow-up
// never touches the workspace row or the secret store (and never emits their
// warning logs) just to be dropped.
func (a *lifecycleAdapter) messageMetadata(ctx context.Context, orgID, eventID string) (SlackMessageMetadata, bool) {
	metaJSON, err := a.stores.Events.GetMetadataSystem(ctx, orgID, eventID)
	if err != nil {
		slackLog.Warn("slack lifecycle: load message metadata failed", "error", err)
		return SlackMessageMetadata{}, false
	}
	if metaJSON == "" {
		return SlackMessageMetadata{}, false
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		slackLog.Warn("slack lifecycle: parse message metadata failed", "error", err)
		return SlackMessageMetadata{}, false
	}
	return meta, true
}

// resolveBotToken resolves the (workspace, api_app_id) pair to its bot
// token. ok=false covers both "no such connected workspace" and "no/empty
// token stored" — neither is an error worth surfacing past a log line, since
// every caller here is best-effort.
func (a *lifecycleAdapter) resolveBotToken(ctx context.Context, orgID, workspaceID, apiAppID string) (string, bool) {
	bundle := slackstore.FromStores(a.stores)
	if bundle == nil {
		return "", false
	}
	ws, err := bundle.Workspaces.GetByWorkspaceAppSystem(ctx, workspaceID, apiAppID)
	if err != nil {
		slackLog.Warn("slack lifecycle: resolve workspace failed", "error", err)
		return "", false
	}
	if ws == nil {
		slackLog.Warn("slack lifecycle: no connected workspace for (workspace, app), dropping", "workspace", workspaceID, "app", apiAppID)
		return "", false
	}
	token, err := a.stores.Secrets.GetSystem(ctx, orgID, ws.BotTokenRef)
	if err != nil {
		slackLog.Warn("slack lifecycle: read bot token failed", "error", err)
		return "", false
	}
	if token == "" {
		slackLog.Warn("slack lifecycle: workspace has no bot token configured, dropping", "workspace", workspaceID, "app", apiAppID)
		return "", false
	}
	return token, true
}

// --- conversation-status / conversation-activity consumer: setStatus lifecycle + failure note ---

// conversationEntry is the per-conversationID cache entry: the resolved slack-or-not verdict
// (and, when true, the channel/thread/token context a worker needs), plus
// whichever per-conversation status worker is currently live. Owned exclusively by
// the single dispatch goroutine — no locking, since nothing else ever
// touches this map.
type conversationEntry struct {
	isSlack           bool
	channel, threadTS string
	botToken          string
	worker            *conversationStatusWorker
	// agentLive records whether the current worker has seen "running" — a
	// worker started by a pre-"running" progress status shows setup-phase
	// text that the "running" transition must replace (the phase is over),
	// whereas a redundant "running" on an already-live worker must NOT blow
	// away a more specific activity-derived text (keepAlive only).
	agentLive bool
	// prevDone is the most recently retired worker's done channel — the
	// per-conversation ordering handoff for a resumed conversation. A successor worker gates
	// on it before its first Slack call (conversationStatusWorker.predecessor), so a
	// retiring worker's trailing setStatus("") can never land after the
	// successor's initial status and wipe it. Deliberately NOT cleared when
	// a successor starts: drainLifecycleWorkers also joins it (a retiring
	// worker may have no successor at shutdown), and receiving from an
	// already-closed channel is free. While it is set and NOT yet closed,
	// pruneConversationCache must not evict this entry — see its doc.
	prevDone chan struct{}
	// cachedAt is refreshed on every conversation-status touch (handleConversationStatus), not
	// just first resolution, so pruneConversationCache's oldest-first eviction is a
	// real LRU over sentinel activity.
	cachedAt time.Time
}

// isTerminalConversationStatus reports whether status is one of the conversation-lifecycle
// terminals (domain.Conversation's documented vocabulary) or the parked/
// awaiting-input "open" state — both stop the worker and clear the
// indicator; only "failed" additionally posts the failure reply.
func isTerminalConversationStatus(status string) bool {
	return domain.IsTerminalConversationStatus(status) || status == domain.StatusOpen
}

// handleConversationStatus resolves (and caches) the conversation's Slack context on first
// sight, then drives the per-conversation worker off the status value: a
// pre-"running" progress status (queued, or any claim phase — see
// preRunIndicatorText, which owns that set)
// starts the worker with that phase's text — or retexts a live one — so the
// thread shows setup progress before the agent exists; "running" starts the
// worker if setup never emitted (or swaps a setup-phase text for the generic
// working pair, or just keepAlives a worker that was already live); a
// terminal status stops it (clearing the indicator, and on "failed" posting
// the brief failure reply).
//
// Terminal statuses here are not necessarily final for the conversationID: a parked
// ("open") conversation is resumable, and so is a completed+abort conversation
// (internal/delegate/resume.go rebroadcasts "running" for the same conversationID on
// resume). So terminal→running for one conversation is a legitimate sequence, and
// the retiring worker's trailing setStatus("") must not race the successor's
// initial status call. That ordering is enforced per conversation, in the worker
// succession itself — the terminal branch records the retiring worker's
// done channel on the entry, and the successor gates its first Slack call
// on it (conversationStatusWorker.predecessor) — NEVER by blocking this dispatcher
// goroutine on a Slack round trip: it is process-wide (one adapter for all
// orgs), and stalling it on a slow/429'd teardown call would head-of-line
// block every other org's sentinels behind one conversation's cleanup.
func (a *lifecycleAdapter) handleConversationStatus(ctx context.Context, evt domain.Event, conversations map[string]*conversationEntry) {
	var meta events.SystemConversationStatusMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		slackLog.Warn("slack lifecycle: decode conversation status failed", "error", err)
		return
	}

	entry, ok := conversations[meta.ConversationID]
	if !ok {
		entry = a.resolveConversationEntry(ctx, evt.OrgID, meta.ConversationID)
		conversations[meta.ConversationID] = entry
		pruneConversationCache(conversations)
	} else {
		// LRU touch (before the isSlack early-return — non-Slack entries are
		// the bulk of the cache and exactly what it exists to avoid
		// re-resolving): without this, cachedAt is "when first resolved" and
		// the pruner would evict by age-since-resolution, hitting long-lived
		// still-active conversations first instead of genuinely idle ones.
		entry.cachedAt = slackLifecycleNow()
	}
	if !entry.isSlack {
		return
	}

	// A worker that reaped itself (idle timeout — its terminal sentinel was
	// dropped, or the conversation sat queued past the timeout) is still referenced
	// here; detect the closed done channel and retire the reference so a
	// later revival (a resume, or an eventual claim after a long queue
	// dwell) starts a fresh worker instead of feeding a dead one.
	if entry.worker != nil {
		select {
		case <-entry.worker.done:
			entry.prevDone = entry.worker.done
			entry.worker = nil
		default:
		}
	}

	switch {
	case meta.Status == "running":
		switch {
		case entry.worker == nil:
			entry.worker = newConversationStatusWorker(ctx, a.client, a.publicURL, evt.OrgID, meta.ConversationID, entry.channel, entry.threadTS, entry.botToken, initialIndicatorText, entry.prevDone)
		case !entry.agentLive:
			// Started by a setup-phase status — that phase is over; show the
			// generic working pair until tool activity refines it.
			entry.worker.sendActivity(initialIndicatorText)
		default:
			entry.worker.keepAlive()
		}
		entry.agentLive = true
	case isTerminalConversationStatus(meta.Status):
		failed := meta.Status == "failed"
		if entry.worker != nil {
			entry.worker.stop(failed)
			entry.prevDone = entry.worker.done
			entry.worker = nil
		} else if failed {
			// The conversation failed before any status-bearing sentinel started a
			// worker — still owe the failure note; nothing to clear since no
			// worker ever set a status.
			postSlackFailureReply(ctx, a.client, a.publicURL, evt.OrgID, meta.ConversationID, entry.channel, entry.threadTS, entry.botToken)
		}
	default:
		if text, ok := preRunIndicatorText(meta.Status); ok {
			if entry.worker == nil {
				entry.worker = newConversationStatusWorker(ctx, a.client, a.publicURL, evt.OrgID, meta.ConversationID, entry.channel, entry.threadTS, entry.botToken, text, entry.prevDone)
				entry.agentLive = false
			} else {
				entry.worker.sendActivity(text)
			}
		}
	}
}

// handleConversationActivity forwards a tool-activity sentinel to its
// conversation's live
// worker, if any. A conversation with no cached entry yet (activity arrived before
// this adapter ever saw a system:conversation:status for it) or no active worker
// (not "running", or not a Slack conversation at all) is silently ignored — the
// verdict-priming event is always system:conversation:status, per the cache contract
// above.
func (a *lifecycleAdapter) handleConversationActivity(ctx context.Context, evt domain.Event, conversations map[string]*conversationEntry) {
	var meta events.SystemConversationActivityMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		slackLog.Warn("slack lifecycle: decode conversation activity failed", "error", err)
		return
	}
	entry, ok := conversations[meta.ConversationID]
	if !ok || !entry.isSlack || entry.worker == nil {
		return
	}
	entry.worker.sendActivity(activityIndicatorText(meta.Tools))
}

// resolveConversationEntry runs the documented chain — Conversations.GetSystem -> Task ->
// Events.GetMetadataSystem — to decide whether conversationID belongs to a Slack
// task and, if so, resolve the thread context + bot token a worker needs.
// Every "this is not a Slack conversation" outcome (no task, wrong entity source,
// wrong event type, no metadata, unresolvable workspace/token) returns the
// same isSlack=false zero value; a genuine store failure logs and also
// degrades to isSlack=false rather than leaving the conversation unresolved forever
// (a transient failure here would otherwise never get a chance to heal,
// since resolution only ever runs once per conversationID).
func (a *lifecycleAdapter) resolveConversationEntry(ctx context.Context, orgID, conversationID string) *conversationEntry {
	entry := &conversationEntry{cachedAt: slackLifecycleNow()}

	conv, err := a.stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		slackLog.Warn("slack lifecycle: load conversation failed", "conversation", conversationID, "error", err)
		return entry
	}
	if conv == nil || conv.TaskID == "" {
		return entry
	}

	task, err := a.stores.Tasks.GetSystem(ctx, orgID, conv.TaskID)
	if err != nil {
		slackLog.Warn("slack lifecycle: load task failed", "conversation", conversationID, "error", err)
		return entry
	}
	if task == nil || task.EntitySource != "slack" || task.EventType != domain.EventSlackMessage || task.PrimaryEventID == "" {
		return entry
	}

	metaJSON, err := a.stores.Events.GetMetadataSystem(ctx, orgID, task.PrimaryEventID)
	if err != nil {
		slackLog.Warn("slack lifecycle: load event metadata failed", "conversation", conversationID, "error", err)
		return entry
	}
	if metaJSON == "" {
		return entry
	}
	var meta SlackMessageMetadata
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		slackLog.Warn("slack lifecycle: parse event metadata failed", "conversation", conversationID, "error", err)
		return entry
	}

	token, ok := a.resolveBotToken(ctx, orgID, meta.WorkspaceID, meta.APIAppID)
	if !ok {
		return entry
	}

	threadTS := meta.ThreadTS
	if threadTS == "" {
		threadTS = meta.TS
	}
	entry.isSlack = true
	entry.channel = meta.Channel
	entry.threadTS = threadTS
	entry.botToken = token
	return entry
}

// pruneConversationCache bounds the cache once it exceeds slackLifecycleCacheMaxEntries:
// evict the oldest (by cachedAt, refreshed on every conversation-status touch — LRU)
// entries with no live worker until the cache is back at
// slackLifecycleCachePruneTarget. This is a genuine size cap, not a
// staleness-only sweep — even entries seen moments ago are evicted if
// that's what it takes to stay under the target, so the map can never grow
// past max regardless of how much traffic arrives within any given window.
//
// Two kinds of entry are never evicted, regardless of age:
//   - one with a live worker — losing it would leak the worker (the
//     dispatcher would no longer know to route its conversation's future sentinels
//     to it);
//   - one whose prevDone hasn't closed yet — a retiring worker is still
//     mid-flight on its trailing setStatus(""), and evicting the entry
//     would drop the only reference to that ordering handle: a resume
//     after eviction would rebuild a fresh entry with a nil predecessor
//     and skip the gate, reintroducing the exact stale-clear-clobbers-
//     resume race the gate exists to close. Once prevDone closes the
//     entry is ordinary again (evicting it is safe — nothing is in
//     flight, so a rebuilt entry's nil predecessor gates nothing real).
//
// If every entry is protected, eviction legitimately can't bring the count
// down further — bounded in practice by concurrent live/retiring workers,
// not total history.
func pruneConversationCache(conversations map[string]*conversationEntry) {
	if len(conversations) <= slackLifecycleCacheMaxEntries {
		return
	}
	type candidate struct {
		id       string
		cachedAt time.Time
	}
	candidates := make([]candidate, 0, len(conversations))
	for id, entry := range conversations {
		if entry.worker != nil {
			continue
		}
		if entry.prevDone != nil {
			select {
			case <-entry.prevDone: // teardown finished — safe to evict
			default:
				continue // retiring worker still mid-flight — keep the handle
			}
		}
		candidates = append(candidates, candidate{id: id, cachedAt: entry.cachedAt})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].cachedAt.Before(candidates[j].cachedAt) })

	for _, c := range candidates {
		if len(conversations) <= slackLifecycleCachePruneTarget {
			return
		}
		delete(conversations, c.id)
	}
}

// indicatorText is one working-indicator update, carrying the same content
// phrased for the two surfaces a setStatus call drives: status follows the
// app's name on the under-composer line ("<app name> is running: Bash"),
// loading stands alone in the in-conversation animated bubble ("Running:
// Bash") — sent as the single loading_messages entry so the bubble mirrors
// live conversation state instead of Slack's stock rotation.
type indicatorText struct {
	status  string
	loading string
}

// initialIndicatorText is the pair a worker shows before any tool-activity
// sentinel has arrived to refine it.
var initialIndicatorText = indicatorText{
	status:  slackLifecycleInitialStatusText,
	loading: slackLifecycleInitialLoadingText,
}

// preRunIndicatorText maps the pre-"running" progress statuses a conversation walks
// through (queue dwell, credential handoff, source-context fetch, worktree
// build, process spawn) to indicator texts, so the thread shows real progress
// from the moment the dispatcher touches the conversation instead of nothing until the
// agent is live. Arms are in lifecycle order.
//
// Copy is deliberately mode-neutral — local mode has no sandbox, so the
// spawn phase says "starting the agent", not "starting the sandbox". The
// credential arm is the one status that cannot occur in local mode at all
// (there is no per-conversation sidecar to seal a bundle for), so it names the wait
// without naming the machinery behind it.
//
// ok=false for any unknown status: future lifecycle states stay invisible
// here rather than rendering guessed copy.
func preRunIndicatorText(status string) (indicatorText, bool) {
	switch status {
	case domain.StatusQueued:
		return indicatorText{status: "is queued…", loading: "Waiting for a free agent slot…"}, true
	case domain.ClaimPhaseAwaitingCredentials:
		return indicatorText{status: "is receiving credentials…", loading: "Receiving credentials…"}, true
	case domain.ClaimPhaseFetching:
		return indicatorText{status: "is gathering task context…", loading: "Gathering task context…"}, true
	case domain.ClaimPhaseCloning:
		return indicatorText{status: "is preparing a workspace…", loading: "Preparing a workspace…"}, true
	case domain.ClaimPhaseAgentStarting:
		return indicatorText{status: "is starting the agent…", loading: "Starting the agent…"}, true
	}
	return indicatorText{}, false
}

// activityIndicatorText derives both indicator texts from a
// system:conversation:activity sentinel's tool list: prefer each tool's Description
// when non-empty, else a humanized form of its Name, joined for the
// (usually single-element, occasionally parallel-tool-call) case.
func activityIndicatorText(tools []events.ConversationActivityTool) indicatorText {
	parts := make([]string, 0, len(tools))
	for _, t := range tools {
		label := t.Description
		if label == "" {
			label = humanizeToolName(t.Name)
		}
		if label != "" {
			parts = append(parts, label)
		}
	}
	if len(parts) == 0 {
		return initialIndicatorText
	}
	joined := strings.Join(parts, ", ")
	return indicatorText{status: "is running: " + joined, loading: "Running: " + joined}
}

// humanizeToolName turns a raw tool name into something readable in a status
// line: strips an MCP "mcp__<server>__" namespace prefix (so the server
// plumbing doesn't leak into user-facing text) and replaces underscores with
// spaces.
func humanizeToolName(name string) string {
	if parts := strings.SplitN(name, "__", 3); len(parts) == 3 && parts[0] == "mcp" {
		name = parts[2]
	}
	return strings.ReplaceAll(name, "_", " ")
}

// postSlackFailureReply posts the brief failure-note thread reply, appending
// a "Details: <url>" fragment built from the multi-mode run URL shape
// (/orgs/{org}/runs/{id}) only when publicURL() is non-empty — per the
// ticket, omit the link entirely rather than guess a wrong fallback.
func postSlackFailureReply(ctx context.Context, client *http.Client, publicURL func() string, orgID, conversationID, channel, threadTS, botToken string) {
	text := slackLifecycleFailureCopy
	if u := publicURL(); u != "" {
		text += fmt.Sprintf(" Details: %s/orgs/%s/runs/%s", u, orgID, conversationID)
	}
	if _, err := slackChatPostMessage(ctx, client, botToken, slackMessageParams{
		Channel: channel, ThreadTS: threadTS, Text: text,
	}); err != nil {
		slackLog.Warn("slack lifecycle: failure reply post failed", "conversation", conversationID, "error", err)
	}
}

// --- per-conversation status worker ---

// conversationStatusWorker owns one live conversation's assistant-status lifecycle: an initial
// setStatus, a refresh ticker (the dead-man switch keeping it inside Slack's
// 2-minute expiry), activity-driven updates debounced to at most one call
// per slackLifecycleActivityDebounce, an idle-timeout reaper for a dropped
// terminal sentinel, and a final clear (+ optional failure reply) on stop.
// Every field is set once at construction and read-only thereafter except
// from loop's own goroutine — all mutation happens by sending on the
// channels below, never by a caller reaching into the struct.
type conversationStatusWorker struct {
	ctx                         context.Context
	client                      *http.Client
	publicURL                   func() string
	orgID, conversationID       string
	channel, threadTS, botToken string
	// initial is the first indicator text the worker shows — the generic
	// working pair for a worker started by "running", a setup-phase pair for
	// one started by a pre-"running" progress status.
	initial indicatorText
	// predecessor, when non-nil, is the retiring previous worker's done
	// channel for the SAME conversation (park → resume) — loop gates on it before its
	// first Slack call, so the predecessor's trailing setStatus("") always
	// resolves before this worker's initial status. nil for a conversation's first
	// worker.
	predecessor <-chan struct{}

	activityCh  chan indicatorText // latest-wins (sendActivity drains-then-pushes)
	keepAliveCh chan struct{}
	stopCh      chan bool // carries "failed"
	done        chan struct{}
}

// newConversationStatusWorker constructs and starts a worker. Its initial setStatus
// is the first Slack call it makes — after the predecessor gate, before it
// ever selects on the command channels — so it's ordered ahead of any
// activity/keepAlive/stop command sent afterward AND behind everything a
// retiring predecessor still had in flight. That ordering is internal to
// the worker only: go w.loop() returns immediately, so newConversationStatusWorker
// itself returns with no guarantee the initial call has started (or even
// been scheduled) yet — callers that need to observe it must poll, not
// assume synchronous completion.
func newConversationStatusWorker(ctx context.Context, client *http.Client, publicURL func() string, orgID, conversationID, channel, threadTS, botToken string, initial indicatorText, predecessor <-chan struct{}) *conversationStatusWorker {
	w := &conversationStatusWorker{
		ctx: ctx, client: client, publicURL: publicURL,
		orgID: orgID, conversationID: conversationID, channel: channel, threadTS: threadTS, botToken: botToken,
		initial:     initial,
		predecessor: predecessor,
		activityCh:  make(chan indicatorText, 1),
		keepAliveCh: make(chan struct{}, 1),
		stopCh:      make(chan bool, 1),
		done:        make(chan struct{}),
	}
	go w.loop()
	return w
}

// sendActivity delivers text as the latest desired indicator text, coalescing
// with any not-yet-consumed prior value — the channel-level half of "at most
// one call per ~3s per conversation (coalesce; latest wins)"; the debounce timer
// inside loop is the other half (bounding the *outgoing* Slack call rate,
// not just the queue depth).
func (w *conversationStatusWorker) sendActivity(text indicatorText) {
	for {
		select {
		case w.activityCh <- text:
			return
		default:
			select {
			case <-w.activityCh:
			default:
			}
		}
	}
}

// keepAlive resets the worker's idle timer without changing its displayed
// text — sent on a repeated "running" status for a conversation whose worker already
// exists, so a resumed conversation's redundant status event doesn't blow away a more
// specific activity-derived text. Non-blocking: a dropped keepalive is
// harmless, the next status/activity event (or the idle reap itself) covers
// it.
func (w *conversationStatusWorker) keepAlive() {
	select {
	case w.keepAliveCh <- struct{}{}:
	default:
	}
}

// stop signals the worker to clear its status (and, if failed, post the
// failure reply) and exit. Fire-and-forget — the teardown Slack calls run
// on the worker's own goroutine, never the dispatcher's: blocking here
// would stall the process-wide dispatch loop (one adapter serves every
// org) on a single conversation's Slack round trip, up to the 429 Retry-After wait.
// The ordering hazard that fire-and-forget would otherwise open — a
// resumed conversation's successor worker setting its initial status while this
// worker's trailing clear is still in flight, then the stale clear landing
// last and wiping it — is closed on the successor's side instead: the
// dispatcher records w.done as the entry's prevDone, and the successor
// gates its first Slack call on it (see conversationStatusWorker.predecessor).
func (w *conversationStatusWorker) stop(failed bool) {
	select {
	case w.stopCh <- failed:
	default:
	}
}

// loop is the worker's whole lifecycle. ctx cancellation (adapter shutdown)
// and an explicit stop() both end it the same way: clear the status, close
// done. Only an explicit stop(true) additionally posts the failure reply —
// a shutdown mid-engagement is not a conversation failure.
//
// Activity debounce is pure trailing-edge: the first activity event in a
// quiet conversation opens a slackLifecycleActivityDebounce window; every further
// event before it elapses just updates the pending text (coalesce, latest
// wins) WITHOUT restarting the window, so a rapid burst — however many
// events — always collapses to exactly one setStatus call, fired once the
// window elapses, and a new window only opens on the next event after that.
// Not restarting the window also bounds worst-case latency: activity that
// keeps arriving faster than the window can never starve it into silence.
func (w *conversationStatusWorker) loop() {
	defer close(w.done)

	// Predecessor gate: on a resumed conversation, wait for the retiring worker's
	// goroutine to fully finish (its trailing setStatus("") resolved, its
	// failure reply posted if any) before making this worker's first Slack
	// call — the per-conversation ordering guarantee that lets stop() stay
	// fire-and-forget on the dispatcher. A stop or ctx cancel arriving while
	// still gated exits without touching Slack at all: this worker never set
	// anything, so there is nothing to clear (a failed-while-gated stop still
	// owes the failure reply — a message, not a status, so it can't be
	// clobbered by the predecessor's in-flight clear).
	if w.predecessor != nil {
		select {
		case <-w.predecessor:
		case failed := <-w.stopCh:
			// Still gated: honor the chain before releasing our own done — a
			// successor gated on OUR done (entry.prevDone) must never run
			// ahead of OUR predecessor's still-in-flight trailing clear.
			// Bounded by ctx.Done() so adapter shutdown can't hang here.
			select {
			case <-w.predecessor:
			case <-w.ctx.Done():
			}
			if failed {
				w.postFailureReply()
			}
			return
		case <-w.ctx.Done():
			return
		}
	}

	text := w.initial
	w.setStatus(text)

	ticker := time.NewTicker(slackLifecycleStatusRefreshInterval)
	defer ticker.Stop()
	idle := time.NewTimer(slackLifecycleIdleTimeout)
	defer idle.Stop()

	var debounce *time.Timer
	var debounceC <-chan time.Time
	var pendingText indicatorText
	var hasPending bool
	// debounce is reassigned throughout the loop (nil between windows); this
	// defer closes over the variable itself, so it stops whatever timer is
	// currently pending at whichever exit path returns — otherwise a debounce
	// window open at ctx-cancel/stop/idle-reap time would leak until it fires
	// on its own.
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-w.ctx.Done():
			w.clearStatus()
			return

		case newText := <-w.activityCh:
			resetLifecycleTimer(idle, slackLifecycleIdleTimeout)
			pendingText, hasPending = newText, true
			if debounceC == nil {
				debounce = time.NewTimer(slackLifecycleActivityDebounce)
				debounceC = debounce.C
			}

		case <-w.keepAliveCh:
			resetLifecycleTimer(idle, slackLifecycleIdleTimeout)

		case failed := <-w.stopCh:
			w.clearStatus()
			if failed {
				w.postFailureReply()
			}
			return

		case <-debounceC:
			debounce, debounceC = nil, nil
			if hasPending {
				text, hasPending = pendingText, false
				w.setStatus(text)
			}

		case <-ticker.C:
			w.setStatus(text)

		case <-idle.C:
			w.clearStatus()
			return
		}
	}
}

// setStatus is the worker's only Slack call for the indicator itself —
// best-effort, a failure just logs (the next ticker tick or activity update
// retries naturally). text.loading rides along as the single
// loading_messages entry, so the in-conversation bubble shows the same live
// state as the status line rather than Slack's default rotation.
func (w *conversationStatusWorker) setStatus(text indicatorText) {
	var loading []string
	if text.loading != "" {
		loading = []string{text.loading}
	}
	if err := slackAssistantSetStatus(w.ctx, w.client, w.botToken, w.channel, w.threadTS, text.status, loading); err != nil {
		slackLog.Warn("slack lifecycle: setStatus failed", "conversation", w.conversationID, "error", err)
	}
}

// clearStatus wipes the indicator on both surfaces — an empty status with no
// loading_messages is Slack's documented "clear it" call.
func (w *conversationStatusWorker) clearStatus() { w.setStatus(indicatorText{}) }

func (w *conversationStatusWorker) postFailureReply() {
	postSlackFailureReply(w.ctx, w.client, w.publicURL, w.orgID, w.conversationID, w.channel, w.threadTS, w.botToken)
}

// resetLifecycleTimer safely stops-drains-resets a running *time.Timer —
// the standard idiom for reusing a timer across multiple select iterations.
func resetLifecycleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
