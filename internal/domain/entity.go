package domain

import (
	"strings"
	"time"
)

// SlackSourceID builds the entities.source_id for a Slack thread:
// "<channel_id>/<thread_ts>". It is the Slack analogue of the tracker's
// ghSourceID ("owner/repo#N") and Jira's raw issue key — the natural-key
// vocabulary that maps a (source, scope, source_id) key onto exactly one
// active entities row (source='slack', kind='thread' when the bot is the reason the thread
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
// already shares. channel+ts names one thread across all of Slack: a shared
// channel keeps its id in every workspace it is in, so the entity's scope is
// SlackScope, never a workspace. The id itself can change (Slack gives a
// private channel a new one when it is shared through Slack Connect), and
// ee/slack follows that by rekeying the channel's threads.
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
// entity it concerns, less the scope, which the caller resolves for the org
// (entityscope.Of): Source is the entities.source column (== provider for the
// mapped providers), SourceID is the natural key (== target, except for Jira,
// where it is the target folded to its canonical spelling), ExternalID is the
// provider id the entity is identified by, and kind is the entities.kind.
// ok=false for anything the touched/produced rule skips — a repo-level GitHub
// target (owner/repo with no '#N'), an empty key, a Jira or Linear coordinate
// with no issue id, or an unmapped provider — so the caller resolves, creates,
// and records nothing.
//
// It is the single home of the (provider, target) → entity mapping shared by
// the exec-funnel touch resolver (resolveTouchedEntityInfo) and the
// conversation-end produced-artifact attach (memoryentities.Attach). GitHub
// targets must parse as owner/repo#N; Jira targets are issue keys; Linear
// targets are issue identifiers; Slack targets are a SlackSourceID
// channel/root_ts.
//
// A Jira coordinate needs externalID, the issue's numeric id, and a Linear
// coordinate the issue's UUID: an issue key changes when the issue moves or
// its project's (or team's) key is renamed, so an entity found or created on
// the key alone can be another issue's, or a second row for this one. GitHub
// and Slack ignore externalID.
//
// NOTE: this assumes every GitHub target is a PR (kind="pr"). Exec only writes
// PRs/reviews today, so that holds — but a GitHub *issue* shares the
// "owner/repo#N" shape, and the poller's stub enrichment would then 404 against
// /pulls/{n} every cycle. GitHub issue support must branch on kind here (and
// give the poller an issue-aware enrichment path).
func EntityRefForExternal(provider, target, externalID string) (ref EntityRef, kind string, ok bool) {
	switch provider {
	case ArtifactProviderGitHub:
		// owner/repo#N is a PR entity; a bare owner/repo is a repo-level
		// action (a branch push's shape too) and maps to no entity.
		if _, _, _, parsed := ParsePRTarget(target); !parsed {
			return EntityRef{}, "", false
		}
		return EntityRef{Source: provider, SourceID: target}, "pr", true
	case ArtifactProviderJira:
		// Normalized rather than passed through: this is the single seam
		// every touched/produced Jira entity resolves through, so folding
		// here is what makes a non-canonical source_id unrepresentable no
		// matter which caller supplied the target. See NormalizeJiraKey.
		key := NormalizeJiraKey(target)
		if key == "" || externalID == "" {
			return EntityRef{}, "", false
		}
		return EntityRef{Source: provider, SourceID: key, ExternalID: externalID}, "issue", true
	case ArtifactProviderLinear:
		// The identifier is the key a person reads and the UUID is the
		// identity; an identifier alone can name another issue once the one it
		// named moved, so nothing is resolved without the UUID.
		identifier := strings.TrimSpace(target)
		if identifier == "" || externalID == "" {
			return EntityRef{}, "", false
		}
		return EntityRef{Source: provider, SourceID: identifier, ExternalID: externalID}, "issue", true
	case ArtifactProviderSlack:
		if target == "" {
			return EntityRef{}, "", false
		}
		return EntityRef{Source: provider, SourceID: target}, "message", true
	default:
		return EntityRef{}, "", false
	}
}

// EntityURL is the link an entity of source with key sourceID in scope is
// stored under, where TF builds it from the key rather than reading it off the
// provider: a Jira issue's {site}/browse/{KEY}, the same link the poller
// stamps. "" for every other source, whose callers supply the url they have.
func EntityURL(source, scope, sourceID string) string {
	if source == ArtifactProviderJira && scope != "" && sourceID != "" {
		return JiraIssueURL(scope, sourceID)
	}
	return ""
}

// JiraIssueURL is the human-facing link to the Jira issue key on site.
func JiraIssueURL(site, key string) string {
	return strings.TrimRight(site, "/") + "/browse/" + key
}

// Entity is a long-lived source object (PR, issue, epic, message). Lives from
// first-poll until closed/merged. All events, tasks, and conversations hang off it.
// Mirrors the `entities` table.
//
// Its natural key is (org, source, scope, source_id), and the key is unique
// among ACTIVE rows only, so a key a provider frees and reuses never collides
// with the closed history it used to name. Where the provider has an id that a
// key change does not move, ExternalID holds it, and that is the identity:
// (org, source, scope, external_id) is unique, and a key change is a rename of
// the row carrying the id (db.EntityStore.RenameSystem), never a second row.
// That is the repository model — the slug is what everything reads, the
// provider id beside it is what a rename is detected by.
type Entity struct {
	ID     string `json:"id"`
	Source string `json:"source"` // "github" | "jira" | "linear" | "slack"
	// Scope is the provider namespace SourceID and ExternalID are unique
	// within: the GitHub host, the Jira site, the Linear workspace id, or
	// SlackScope for every Slack entity. Computed from org settings by
	// EntityScope.
	Scope    string `json:"scope"`
	SourceID string `json:"source_id"` // "owner/repo#18", a Jira issue key, a Linear identifier, etc. — the key it answers to now
	// ExternalID is the provider's stable id for the object — a Jira issue's
	// numeric id, a Linear issue's UUID. Empty when the source has none or TF
	// has not learned it (a Jira entity created before issue ids were
	// recorded, until a response names its id).
	ExternalID string `json:"external_id,omitempty"`
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
	Description  string     `json:"description"`   // flattened, capped body preview; full-body hash is diffed in the snapshot
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
