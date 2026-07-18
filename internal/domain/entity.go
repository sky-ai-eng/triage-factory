package domain

import "time"

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

// Entity is a long-lived source object (PR, issue, epic, message). Lives from
// first-poll until closed/merged. All events, tasks, and runs hang off it.
// Mirrors the `entities` table in internal/db/db.go.
type Entity struct {
	ID       string `json:"id"`
	Source   string `json:"source"`    // "github" | "jira" | "linear" | "slack"
	SourceID string `json:"source_id"` // "owner/repo#18", a Jira issue key, etc.
	// Kind is "pr" | "issue" | "epic" for the poller-backed sources. For
	// Slack, kind encodes thread engagement: "thread" when the bot is why
	// the thread exists (its root message was a mention, or a run posted
	// the root itself), "message" when it's a mid-thread summons into
	// someone else's thread. Set once at creation from whichever caller
	// first resolves the entity (ingest.go's root-mention check, or exec
	// slack send's root-post op) and never rewritten afterward.
	Kind         string  `json:"kind"`
	Title        string  `json:"title"`
	URL          string  `json:"url"`
	SnapshotJSON string  `json:"snapshot_json"`        // opaque poller state — diff scope only, kept small
	Description  string  `json:"description"`          // flattened issue/PR body; NOT diffed
	State        string  `json:"state"`                // "active" | "closed"
	ProjectID    *string `json:"project_id,omitempty"` // nil = unassigned. FK ON DELETE SET NULL.
	// ClassificationRationale is the highest-scoring project's one-sentence
	// rationale from the classifier, regardless of whether the
	// score crossed threshold. Empty until the classifier has run for
	// this entity. Useful for surfacing "why is this unassigned?" in UI.
	ClassificationRationale string     `json:"classification_rationale,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	LastPolledAt            *time.Time `json:"last_polled_at"`
	ClosedAt                *time.Time `json:"closed_at"`
	// PollSeq backs the tracker's snapshot-write CAS (TFAC-579):
	// UpdateSnapshotCASSystem bumps it by 1 on every successful write and
	// requires the caller's last-observed value to still match, so a
	// straggler ex-leader's late write (stale PollSeq) becomes a harmless
	// no-op instead of overwriting a newer snapshot and losing a
	// transition. Populated on every read; callers thread it straight back
	// into their next CAS call.
	PollSeq int64 `json:"poll_seq"`
}
