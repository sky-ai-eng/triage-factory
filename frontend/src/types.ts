export type TaskSource = 'github' | 'jira'
export type EntityKind = 'pr' | 'issue' | 'epic' | 'message'

export interface Task {
  id: string
  entity_id: string
  source: TaskSource
  source_id: string
  source_url: string
  title: string
  entity_kind: EntityKind
  event_type: string
  dedup_key?: string
  severity?: string
  relevance_reason?: string
  scoring_status: string
  created_at: string
  status: string
  priority_score: number | null
  autonomy_suitability: number | null
  ai_summary?: string
  priority_reasoning?: string
  close_reason?: string
  // RFC3339 timestamp indicating when the snoozed task wakes;
  // absent/empty when it isn't snoozed. Snoozed tasks are unclaimed:
  // the server refuses snoozing a claimed task, and claiming a snoozed
  // task wakes it by clearing this field.
  snooze_until?: string
  // Non-zero when the Jira entity has open subtasks (status not in
  // Done.Members). UI surfaces a "consider decomposing" hint when set —
  // the task was created before subtasks appeared, or the user added them
  // after starting work. Always 0 for GitHub tasks.
  open_subtask_count?: number
  // SKY-330: claim cols, exposed so the assignee picker on the board
  // can render current state without a second fetch. Exactly one is
  // set when claimed; both absent (omitempty on the wire) when
  // unclaimed. The XOR is enforced server-side.
  claimed_by_agent_id?: string
  claimed_by_user_id?: string
  // The task's owning team. Surfaced so the multi-team board can
  // color-code / tag rows by team. The frontend only renders the tag
  // when the viewer belongs to ≥2 teams.
  // TODO(SKY-379): board row color-coding consumes this.
  team_id?: string
}

// TeamSummary is one entry of GET /api/teams — the identity row the
// multi-team selectors enumerate. The team count drives
// whether a team control renders at all (the ≥2 gate).
export interface TeamSummary {
  id: string
  name: string
  slug: string
}

// TeamsResponse is GET /api/teams: the viewer's teams in the active org
// plus their sticky default (last_acting_team_id), present only when it is
// still one of those teams.
export interface TeamsResponse {
  teams: TeamSummary[]
  last_acting_team_id?: string
}

// TeamBot mirrors the bot half of /api/team/members (SKY-330). Null
// when no agent is bootstrapped OR team_agents.enabled is false for
// the caller's team — same gate the swipe-delegate handler enforces.
// Frontend hides the Bot row in the picker when this is null. The
// per-user TeamMember + TeamMembersResponse shapes live further down
// (where they were originally declared for the predicate editor).
export interface TeamBot {
  agent_id: string
  display_name: string
}

export interface AgentRun {
  ID: string
  TaskID: string
  Status: string
  Model: string
  StartedAt: string
  CompletedAt?: string
  TotalCostUSD?: number
  DurationMs?: number
  NumTurns?: number
  StopReason?: string
  ResultSummary: string
  SessionID?: string
  WorktreePath?: string
  // pending_kind is set by the server's runResponse projection when
  // status == 'pending_approval'. The discriminator tells the Board
  // which approval card variant to render: a queued review opens
  // ReviewOverlay (with inline-comment editing); a queued PR opens
  // PendingPROverlay (title/body editor + Open-PR button). Empty /
  // undefined for non-pending runs.
  pending_kind?: 'review' | 'pr'
  chain_run_id?: string
  chain_step_index?: number | null
}

export interface HeldTakeover {
  run_id: string
  session_id: string
  takeover_path: string
  task_title: string
  source_id: string
  taken_over_at: string
  resume_command: string
}

export interface AgentMessage {
  ID: number
  RunID: string
  Role: string
  Content: string
  Subtype: string
  ToolCalls?: ToolCall[]
  ToolCallID: string
  IsError: boolean
  Model: string
  InputTokens?: number
  OutputTokens?: number
  CacheReadTokens?: number
  CacheCreationTokens?: number
  CreatedAt: string
}

export interface ToolCall {
  id: string
  name: string
  input: Record<string, unknown>
}

// CuratorMessage / CuratorRequest mirror the Go domain types in
// internal/domain/curator.go. The Go structs carry json tags so the
// wire shape is snake_case — diverging from AgentMessage, which is
// PascalCase because its Go struct has no tags. Don't try to unify.
export interface CuratorMessage {
  id: number
  request_id: string
  role: string // "assistant" | "user" | "tool" | "system"
  subtype: string // "" | "context_change" | "yield_request" | ...
  content: string
  tool_calls?: ToolCall[]
  tool_call_id?: string
  is_error?: boolean
  metadata?: Record<string, unknown>
  model?: string
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_creation_tokens?: number
  created_at: string
}

export type CuratorRequestStatus = 'queued' | 'running' | 'done' | 'cancelled' | 'failed'

export interface CuratorRequest {
  id: string
  project_id: string
  status: CuratorRequestStatus
  user_input: string
  error_msg?: string
  cost_usd: number
  duration_ms: number
  num_turns: number
  started_at?: string
  finished_at?: string
  created_at: string
}

// History endpoint envelope: each request carries its message stream
// inline. Frontend dedupes incoming WS messages against this.
export interface CuratorRequestWithMessages extends CuratorRequest {
  messages: CuratorMessage[]
}

export interface TriageEvent {
  id?: number
  event_type: string
  task_id: string
  source_id: string
  metadata: string
  created_at: string
}

/** Shape of `data` on a `{ type: "event" }` WS frame — matches Go's
 *  `domain.Event`. The frontend factory uses entity_id + event_type
 *  to drive chip animations between stations. */
export interface DomainEvent {
  id?: string
  event_type: string
  /** FK to entities.id; null for system events (poll markers, etc.). */
  entity_id?: string | null
  dedup_key?: string
  metadata_json?: string
  occurred_at?: string
  created_at?: string
}

export type PromptKind = 'leaf' | 'chain'

export interface Prompt {
  id: string
  name: string
  body: string
  source: string
  kind: PromptKind
  usage_count: number
  created_at: string
  updated_at: string
}

export interface ChainStep {
  chain_prompt_id: string
  step_index: number
  step_prompt_id: string
  brief: string
  created_at: string
}

export type ChainVerdictOutcome = 'advance' | 'abort' | 'final'

export interface ChainVerdict {
  outcome: ChainVerdictOutcome
  reason: string
  notes?: string
  // synthetic is internal-only (json:"-" in Go) — not on the wire
}

export interface ChainRunStepView {
  step: ChainStep
  run?: AgentRun
  verdict?: ChainVerdict
}

export interface ChainRunResponse {
  chain_run: ChainRun
  steps: ChainRunStepView[]
}

export interface ChainRun {
  id: string
  chain_prompt_id: string
  task_id: string
  trigger_type: string
  trigger_id?: string
  status: 'running' | 'completed' | 'aborted' | 'failed' | 'cancelled'
  abort_reason?: string
  aborted_at_step?: number
  worktree_path: string
  started_at: string
  completed_at?: string
}

export interface EventType {
  id: string
  source: string
  category: string
  label: string
  description: string
}

// Event handlers (SKY-259) — unified successor to the former TaskRule
// + PromptTrigger types. The backend stores both in one event_handlers
// table; the FE keeps split UI pages but consumes the discriminated
// union below. Each member is fully typed: TS catches cross-kind
// access at compile time, no nullable per-kind fields leak out of the
// discriminator.

interface EventHandlerBase {
  id: string
  event_type: string
  scope_predicate_json: string | null
  enabled: boolean
  source: 'system' | 'user'
  created_at: string
  updated_at: string
}

export interface RuleHandler extends EventHandlerBase {
  kind: 'rule'
  name: string
  default_priority: number
  sort_order: number
}

export interface TriggerHandler extends EventHandlerBase {
  kind: 'trigger'
  prompt_id: string
  trigger_type: string
  breaker_threshold: number
  min_autonomy_suitability: number
}

export type EventHandler = RuleHandler | TriggerHandler

export function isRule(h: EventHandler): h is RuleHandler {
  return h.kind === 'rule'
}
export function isTrigger(h: EventHandler): h is TriggerHandler {
  return h.kind === 'trigger'
}

export interface FieldSchema {
  name: string
  type: 'bool' | 'string' | 'int' | 'string_list'
  enum_values?: string[]
  description?: string
}

export interface EventSchema {
  event_type: string
  fields: FieldSchema[]
}

/** GET /api/config response. One-shot read at FE boot — AuthGate uses
 *  deployment_mode to choose between the local keychain-capture flow
 *  and the multi-mode OAuth flow. Per-user identity that used to live
 *  here (github_username, jira_*) moved to /api/me. */
export interface DeploymentConfig {
  deployment_mode: 'local' | 'multi'
}

/** AuthOrg is one membership row in MeResponse.orgs. Standalone export
 *  so multi-mode consumers (OrgPicker, OrgContext) can name the type
 *  without round-tripping through MeResponse['orgs'][number]. */
export interface AuthOrg {
  id: string
  name: string
  role: string
}

/** GET /api/me response — the canonical "current user" shape, served in
 *  both modes (local mode synthesizes from the users row, multi mode
 *  reads via JWT-context query). All fields except `id` and `orgs` are
 *  optional because the server uses `omitempty` for unset values; FE
 *  consumers should treat empty/undefined identically.
 *
 *  Single source of truth — every endpoint that surfaces the current
 *  user's identity goes through here. */
export interface MeResponse {
  id: string
  email?: string
  display_name?: string
  avatar_url?: string
  github_username?: string
  /** Atlassian account ID. Absent when Jira is not yet connected — the
   *  predicate editor renders the Variant-A toggle disabled with a
   *  "configure Jira on Settings" hint. */
  jira_account_id?: string
  /** Jira-side display name. Captured alongside account ID from
   *  /rest/api/2/myself; used in UI hints ("Match my issues as
   *  Aidan Allchin"). Absent when Jira not connected. */
  jira_display_name?: string
  orgs: AuthOrg[]
  /** Session-scoped active org. Per-session, not per-user — two tabs
   *  of the same user can hold different values. Omitted by the server
   *  when the session has no active org (zero memberships, or the
   *  previously-selected org membership was revoked). */
  active_org_id?: string
  /** Instance-level TF_DEFAULT_JOIN_POLICY (SKY-345). Drives the
   *  /no-orgs rare-state copy when the user has zero memberships.
   *  Absent in local mode, where signup-policy doesn't apply. */
  join_policy?: 'personal-org-on-signup' | 'auto-join-default' | 'invite-only'
}

/** GET /api/team/members row. Backs Variant B's searchable multi-select.
 *  Local mode returns a single-entry array containing the synthetic
 *  LocalDefaultUserID; multi mode (post-SKY-251) returns the active
 *  user's team roster. */
export interface TeamMember {
  user_id: string
  display_name: string
  github_username: string | null
  /** Atlassian account ID (SKY-270). Null when this member hasn't
   *  connected Jira yet. */
  jira_account_id: string | null
  is_current_user: boolean
}

export interface TeamMembersResponse {
  members: TeamMember[]
  // SKY-330: bot entry, populated when the caller's team has an
  // enabled agent (otherwise null). Same gate as swipe-delegate.
  bot: TeamBot | null
}

export interface Project {
  id: string
  name: string
  description: string
  /** The team that owns this project (domain.Project.TeamID). The
   *  pinned-repos editor sources its options from this team's tracked
   *  set, since the PATCH validator only accepts repos this team tracks. */
  team_id: string
  curator_session_id?: string
  pinned_repos: string[]
  jira_project_key: string
  linear_project_key: string
  /** Per-project Curator spec-authorship skill (SKY-221). Empty string =
   *  use the seeded `system-ticket-spec` default. The Curator dispatch
   *  materializes whichever prompt this points at as a literal Claude
   *  Code skill at `<cwd>/.claude/skills/ticket-spec/SKILL.md` on every
   *  turn — changes apply immediately without a session reset. */
  spec_authorship_prompt_id: string
  created_at: string
  updated_at: string
}

export interface ProjectExportPreviewFile {
  path: string
  size_bytes: number
}

export interface ProjectExportPreview {
  files: ProjectExportPreviewFile[]
  total_size: number
}

export interface ProjectImportWarning {
  code: string
  repo?: string
  message: string
}

export interface ProjectImportResult {
  project: Project
  warnings: ProjectImportWarning[]
}

export interface ProjectImportError {
  error: string
  message?: string
  missing_repos?: Array<{
    repo: string
    error: string
  }>
}

export interface KnowledgeFile {
  path: string
  /** RFC 6838 content type detected from the filename extension —
   *  drives the panel's render switch (markdown / image / text /
   *  no-preview). "application/octet-stream" for unknown extensions. */
  mime_type: string
  /** Inlined for text-shaped files under ~256KB; empty otherwise.
   *  Frontend lazy-fetches the raw endpoint when content is empty
   *  and a preview is needed. */
  content: string
  updated_at: string
  size_bytes: number
}

export interface KnowledgeUploadResult {
  /** Sanitized server-side filename (may differ from the client's
   *  original if path components were stripped). Empty when the
   *  upload failed. */
  path?: string
  /** Original filename as the client sent it — used in error toasts
   *  so the user can match a failure back to the file they dropped. */
  original: string
  error?: string
}

export interface ToastPayload {
  id: string
  level: 'info' | 'success' | 'warning' | 'error'
  title?: string
  body: string
}

export interface FactoryRecentEvent {
  event_type: string
  /** Source-time when known (commit committed_at, check completed_at,
   *  review submitted_at), falling back to detection time. Drives
   *  chain ORDERING — two events from one poll order by their
   *  upstream timestamps, not their insert order. */
  at: string
  /** Row insert time. Drives chain CLUSTERING — events from a single
   *  poll cycle insert within milliseconds, so a small gap test on
   *  this field separates one poll's burst from the next regardless
   *  of how the upstream timestamps line up. */
  detected_at: string
}

export interface FactoryEntity {
  id: string
  source: string
  source_id: string
  kind: string
  title: string
  url: string
  mine: boolean
  current_event_type?: string
  last_event_at?: string
  /** Last ~10 events for this entity, oldest first. The factory reconciler
   * walks this as an animation chain — a poll cycle that emitted two
   * events in sequence (new_commits → ci_passed) shows both transitions
   * rather than teleporting to the latest. */
  recent_events?: FactoryRecentEvent[]
  // GitHub PR fields.
  number?: number
  repo?: string
  author?: string
  additions?: number
  deletions?: number
  // Jira fields.
  status?: string
  priority?: string
  assignee?: string
  /** Active tasks for this entity, grouped by event_type. Drives the
   *  station drawer's drag-to-delegate flow: dropping on the runs tray
   *  reads the matching event_type's first dedup_key and forwards it
   *  (along with entity_id + event_type) to POST /api/factory/delegate,
   *  which then find-or-creates via the unique index on
   *  (entity_id, event_type, dedup_key). task_id is informational —
   *  not currently sent on the request — and is kept available for
   *  future UI hints (e.g., "this queued chip already has a task"). */
  pending_tasks?: Record<string, Array<{ task_id: string; dedup_key: string }>>
  /** True if any run on this entity is in awaiting_input. Drives the
   *  attention badge on the runs-tray chip so a user scanning the
   *  factory can spot yielded runs without opening each card. */
  has_awaiting_input?: boolean
}

export interface FactoryStation {
  event_type: string
  items_24h: number
  triggered_24h: number
  active_runs: number
  /** From-catalog-start event count for this station's event_type.
   *  This value may be populated for both terminal and non-terminal
   *  stations, depending on the backend snapshot data. */
  items_lifetime: number
  runs: Array<{
    run: AgentRun
    task: Task
    mine: boolean
  }>
}

export interface FactorySnapshot {
  stations: Record<string, FactoryStation>
  entities: FactoryEntity[]
}

export type WSEvent =
  | { type: 'agent_run_update'; run_id: string; data: { status: string } }
  | { type: 'agent_message'; run_id: string; data: AgentMessage }
  | { type: 'curator_message'; project_id: string; data: CuratorMessage }
  | {
      type: 'curator_request_update'
      project_id: string
      data: { request_id: string; status: CuratorRequestStatus }
    }
  | { type: 'project_knowledge_updated'; project_id: string; data: null }
  | {
      type: 'entities_assigned_to_project'
      project_id: string
      data: { entity_ids: string[] }
    }
  | { type: 'curator_reset'; project_id: string; data: null }
  | { type: 'event'; data: DomainEvent }
  | { type: 'tasks_updated'; data: Record<string, never> }
  | {
      // SKY-261 B+: claim-axis change. Exactly one of the two ID fields
      // is populated when claim landed; both empty when claim was
      // cleared (Requeue, revert). Status is NOT in the payload —
      // status changes fire as task_updated. The two channels stay
      // orthogonal, matching the three-axis design.
      type: 'task_claimed'
      data: {
        task_id: string
        claimed_by_agent_id: string
        claimed_by_user_id: string
      }
    }
  | {
      // Genuine status transitions (done, dismissed, snoozed) — NOT
      // responsibility changes. Responsibility lives on task_claimed.
      type: 'task_updated'
      data: { task_id: string; status: string }
    }
  | { type: 'scoring_started'; data: { task_ids: string[] } }
  | { type: 'scoring_completed'; data: { task_ids: string[] } }
  | {
      type: 'repo_docs_updated'
      data: { id: string; has_readme: boolean; has_claude_md: boolean; has_agents_md: boolean }
    }
  | { type: 'repo_profile_updated'; data: { id: string; profile_text: string } }
  | { type: 'toast'; data: ToastPayload }
