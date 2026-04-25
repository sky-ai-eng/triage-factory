// Factory-side presentation metadata for event types.
//
// The predicate field schema (what can be filtered, types, enums, docs) comes
// from the backend via GET /api/event-schemas — reflected from the Go
// predicate structs in internal/domain/events/. That's the authoritative
// source and we fetch it live.
//
// What lives HERE is the presentation layer specific to the factory view:
// category (which row the station belongs to), lifecycle phase (ingress /
// activation / progress / setback / terminal), and a procedural glyph
// identifier that the station renderer draws inside the core chamber.
//
// EventBadge.tsx holds a parallel label/description/color map for the rest
// of the app. We intentionally duplicate labels here rather than cross-
// import — factory has its own typography and tone. If the two maps drift,
// the right fix is extracting a shared single-source map, not auto-syncing.

export type Category =
  | 'pr_flow'
  | 'pr_review'
  | 'pr_ci'
  | 'pr_signals'
  | 'jira_flow'
  | 'jira_signals'

export type Lifecycle = 'ingress' | 'activation' | 'progress' | 'setback' | 'terminal'

export type GlyphKind =
  | 'spark' // fresh arrival (opened, assigned, available)
  | 'unlock' // gateway (ready_for_review, review_requested, became_atomic)
  | 'pulse' // forward work (new_commits, status_changed)
  | 'check' // positive validation (ci_check_passed, review_approved)
  | 'cross' // negative validation (ci_check_failed, review_changes_requested, conflicts)
  | 'tag' // label events
  | 'bubble' // comment / mention events
  | 'merge' // terminal merge
  | 'close' // terminal close / dismiss

export interface FactoryEvent {
  eventType: string
  label: string
  source: 'github' | 'jira' | 'system'
  category: Category
  lifecycle: Lifecycle
  glyph: GlyphKind
  /** Optional color override. Falls back to the category's default tint.
   * Used for events whose positive/negative valence diverges from the
   * category baseline (e.g. approved vs. changes-requested within review). */
  tint?: number
}

// Only populated for the events the POC renders. Adding a station = adding
// an entry here. Predicate fields come from the API, not this table.
export const FACTORY_EVENTS: Record<string, FactoryEvent> = {
  'github:pr:opened': {
    eventType: 'github:pr:opened',
    label: 'PR Opened',
    source: 'github',
    category: 'pr_flow',
    lifecycle: 'ingress',
    glyph: 'spark',
  },
  'github:pr:ready_for_review': {
    eventType: 'github:pr:ready_for_review',
    label: 'Ready for Review',
    source: 'github',
    category: 'pr_flow',
    lifecycle: 'activation',
    glyph: 'unlock',
  },
  'github:pr:new_commits': {
    eventType: 'github:pr:new_commits',
    label: 'New Commits',
    source: 'github',
    category: 'pr_flow',
    lifecycle: 'progress',
    glyph: 'pulse',
  },
  'github:pr:ci_check_passed': {
    eventType: 'github:pr:ci_check_passed',
    label: 'CI Passed',
    source: 'github',
    category: 'pr_ci',
    lifecycle: 'progress',
    glyph: 'check',
  },
  'github:pr:ci_check_failed': {
    eventType: 'github:pr:ci_check_failed',
    label: 'CI Failed',
    source: 'github',
    category: 'pr_ci',
    lifecycle: 'setback',
    glyph: 'cross',
    tint: 0xc46060, // red — failure valence
  },
  'github:pr:merged': {
    eventType: 'github:pr:merged',
    label: 'Merged',
    source: 'github',
    category: 'pr_flow',
    lifecycle: 'terminal',
    glyph: 'merge',
  },
  'github:pr:closed': {
    eventType: 'github:pr:closed',
    label: 'Closed',
    source: 'github',
    category: 'pr_flow',
    lifecycle: 'terminal',
    glyph: 'close',
    tint: 0x8a8480, // neutral gray — abandoned, not a failure
  },
  'github:pr:conflicts': {
    eventType: 'github:pr:conflicts',
    label: 'Conflicts',
    source: 'github',
    category: 'pr_flow',
    lifecycle: 'setback',
    glyph: 'cross',
    tint: 0xb8805a, // warm amber — merge conflicts need resolution
  },
  'github:pr:review_requested': {
    eventType: 'github:pr:review_requested',
    label: 'Review Requested',
    source: 'github',
    category: 'pr_review',
    lifecycle: 'activation',
    glyph: 'bubble',
  },
  'github:pr:review_approved': {
    eventType: 'github:pr:review_approved',
    label: 'Review Approved',
    source: 'github',
    category: 'pr_review',
    lifecycle: 'progress',
    glyph: 'check',
    tint: 0x6ea87a, // sage green — positive review valence
  },
  'github:pr:review_changes_requested': {
    eventType: 'github:pr:review_changes_requested',
    label: 'Changes Requested',
    source: 'github',
    category: 'pr_review',
    lifecycle: 'setback',
    glyph: 'cross',
    tint: 0xb8805a, // warm amber — needs-attention valence
  },
  'github:pr:review_commented': {
    eventType: 'github:pr:review_commented',
    label: 'Review Commented',
    source: 'github',
    category: 'pr_review',
    lifecycle: 'progress',
    glyph: 'bubble',
  },
}
