package domain

import "time"

// OrgEventSource is one org's declared policy for one event source — the
// `org_event_sources` row. Today it carries exactly one fact: whether an org
// admin has paused the source.
//
// Turning a source off suspends the whole source: it is not polled, produces no
// events, mints no tasks, and resolves no credential for an agent. What it does
// NOT do is destroy anything — the credential stays bound, the tracked repos
// and authored handlers stay as they are — which is the entire difference
// between this and unbinding the credential, and what makes it one switch to
// undo.
//
// A source with no row is not disabled. Absence means "no per-source overrides
// recorded" rather than "off" — which is why Disabled is a stored column and
// not the row's own presence.
type OrgEventSource struct {
	OrgID string `json:"org_id"`
	// Kind is the source's registry key ("github", "jira", "slack") — the
	// segment before the first ':' in its event types. Unconstrained by any
	// foreign key: a row naming a source this build does not carry resolves to
	// nothing and is inert, rather than being invalid.
	Kind string `json:"kind"`
	// Disabled is the off switch. Forward-only, like every other tracking
	// change: it stops NEW work — events, tasks, credential resolutions — and
	// destroys no existing task, run, or handler.
	Disabled bool `json:"disabled"`
	// DisabledAt / DisabledBy describe the live pause and are cleared when the
	// source is re-enabled — the history of who paused what is in
	// access_changes, which is the surface built to keep it.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	DisabledBy *string    `json:"disabled_by,omitempty"`
}
