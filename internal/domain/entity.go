package domain

import (
	"strings"
	"time"
)

// SlackSourceID builds the entities.source_id for a Slack thread:
// "<channel_id>/<thread_ts>". It is the Slack analogue of the tracker's
// ghSourceID ("owner/repo#N") and Jira's raw issue key — the natural-key
// vocabulary that maps a (source, source_id) pair onto exactly one entities
// row (source='slack', kind='thread' when the bot is the reason the thread
// exists — its root was a mention or a run's own post — else 'message' for
// a mid-thread summons into someone else's thread; see Entity.Kind).
//
// The thread root is the entity grain. Slack delivers most mentions inside a
// thread (the webhook carries thread_ts); a mention on a *root* channel message
// has no thread_ts, so the caller passes that message's own ts as threadTS —
// "<channel>/<ts>" — making the root message the thread it anchors. Resolving
// thread_ts-or-ts is the caller's job (it owns the webhook payload); this
// helper just pins the format so the resolver (TFAC-513) and the Slack
// thread-create caller (TFAC-510) can't drift on the separator or ordering.
//
// Exported and dependency-free (rather than living next to the unexported
// tracker ghSourceID) precisely because the consumer lives outside the tracker
// package — it sits beside the other entity-key builders the rest of the app
// already shares. channel+ts is unique within a workspace; the org_id column on
// entities (PG's UNIQUE(org_id, source, source_id)) separates workspaces.
func SlackSourceID(channel, threadTS string) string {
	return channel + "/" + threadTS
}

// NormalizeJiraKey folds a Jira issue key to the canonical spelling Jira
// itself answers with: upper-case, no surrounding whitespace.
//
// Jira resolves issue keys case-insensitively on every surface — the REST
// issue endpoints and JQL alike — but always *answers* with the canonical
// key. The poller matches a refresh back to its entity by exact source_id,
// so an entity minted under a caller-supplied spelling that differs from
// the canonical one can never match its own issue again: the refresh comes
// back under the canonical key, the lookup misses, and the entity holds its
// last snapshot forever. Nothing errors — the API call that minted it
// succeeded, and so does every later one — which is what makes normalizing
// at the boundary the fix rather than a validation check somewhere.
func NormalizeJiraKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// EntityRefForExternal maps an external write/artifact coordinate to the
// entity natural key it concerns: source is the entities.source column
// (== provider for the mapped providers), sourceID is the natural key
// (== target, except for Jira, where it is the target folded to its
// canonical spelling), kind is the entities.kind. ok=false for anything the
// touched/produced rule skips — a repo-level GitHub target (owner/repo with
// no '#N'), an empty key, or an unmapped provider — so the caller resolves,
// creates, and records nothing.
//
// It is the single home of the (provider, target) → entity mapping shared by
// the exec-funnel touch resolver (resolveTouchedEntityInfo) and the
// conversation-end produced-artifact attach (the delegate spawner). GitHub
// targets must parse as owner/repo#N; Jira targets are issue keys; Slack
// targets are a SlackSourceID channel/root_ts.
//
// NOTE: this assumes every GitHub target is a PR (kind="pr"). Exec only writes
// PRs/reviews today, so that holds — but a GitHub *issue* shares the
// "owner/repo#N" shape, and the poller's stub enrichment would then 404 against
// /pulls/{n} every cycle. GitHub issue support must branch on kind here (and
// give the poller an issue-aware enrichment path).
func EntityRefForExternal(provider, target string) (source, sourceID, kind string, ok bool) {
	switch provider {
	case ArtifactProviderGitHub:
		// owner/repo#N is a PR entity; a bare owner/repo is a repo-level
		// action (a branch push's shape too) and maps to no entity.
		if _, _, _, parsed := ParsePRTarget(target); !parsed {
			return "", "", "", false
		}
		return provider, target, "pr", true
	case ArtifactProviderJira:
		// Normalized rather than passed through: this is the single seam
		// every touched/produced Jira entity resolves through, so folding
		// here is what makes a non-canonical source_id unrepresentable no
		// matter which caller supplied the target. See NormalizeJiraKey.
		key := NormalizeJiraKey(target)
		if key == "" {
			return "", "", "", false
		}
		return provider, key, "issue", true
	case ArtifactProviderSlack:
		if target == "" {
			return "", "", "", false
		}
		return provider, target, "message", true
	default:
		return "", "", "", false
	}
}

// Entity is a long-lived source object (PR, issue, epic, message). Lives from
// first-poll until closed/merged. All events, tasks, and conversations hang off it.
// Mirrors the `entities` table in internal/db/db.go.
type Entity struct {
	ID       string `json:"id"`
	Source   string `json:"source"`    // "github" | "jira" | "linear" | "slack"
	SourceID string `json:"source_id"` // "owner/repo#18", a Jira issue key, etc.
	// Kind is "pr" | "issue" | "epic" for the poller-backed sources. For
	// Slack, kind encodes thread engagement: "thread" when the bot is why
	// the thread exists (its root message was a mention, or a conversation posted
	// the root itself), "message" when it's a mid-thread summons into
	// someone else's thread. Set once at creation from whichever caller
	// first resolves the entity (ingest.go's root-mention check, or exec
	// slack send's root-post op) and never rewritten afterward.
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	SnapshotJSON string     `json:"snapshot_json"` // opaque poller state — diff scope only, kept small
	Description  string     `json:"description"`   // flattened issue/PR body; NOT diffed
	State        string     `json:"state"`         // "active" | "closed"
	CreatedAt    time.Time  `json:"created_at"`
	LastPolledAt *time.Time `json:"last_polled_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	// PollSeq backs the tracker's snapshot-write CAS (TFAC-579):
	// UpdateSnapshotCASSystem bumps it by 1 on every successful write and
	// requires the caller's last-observed value to still match, so a
	// straggler ex-leader's late write (stale PollSeq) becomes a harmless
	// no-op instead of overwriting a newer snapshot and losing a
	// transition. Populated on every read; callers thread it straight back
	// into their next CAS call.
	PollSeq int64 `json:"poll_seq"`
}
