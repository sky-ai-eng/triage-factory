export type TaskSource = 'github' | 'jira' | 'slack'
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
  // Number of messages addressed to the bot on a Slack thread's entity.
  // A Slack thread carries one long-lived task whose title names only the
  // channel, so the card shows this count as a badge — it rises as
  // follow-ups land while a run is in flight. Absent/0 for non-Slack tasks.
  slack_message_count?: number
  // Claim cols, exposed so the assignee picker on the board
  // can render current state without a second fetch. Exactly one is
  // set when claimed; both absent (omitempty on the wire) when
  // unclaimed. The XOR is enforced server-side.
  claimed_by_agent_id?: string
  claimed_by_user_id?: string
  // The task's owning team. Surfaced so the multi-team board can
  // color-code / tag rows by team. The frontend only renders the tag
  // when the viewer belongs to ≥2 teams.
  // TODO: board row color-coding consumes this.
  team_id?: string
}

// TeamSummary is one entry of GET /api/teams — the identity row the
// multi-team selectors enumerate. The team count drives
// whether a team control renders at all (the ≥2 gate).
export interface TeamSummary {
  id: string
  name: string
  slug: string
  /** The viewer's membership role in this team ("admin" | "member" |
   *  "viewer"). The settings surface renders the Team section only when the
   *  viewer admins ≥1 team and filters its selector to those teams. Local /
   *  N=1 reports "admin" for the sole team. */
  role: string
}

// TeamsResponse is GET /api/teams: the viewer's teams in the active org
// plus their sticky default (last_acting_team_id), present only when it is
// still one of those teams.
export interface TeamsResponse {
  teams: TeamSummary[]
  last_acting_team_id?: string
}

// TeamBot mirrors the bot half of /api/team/members. Null
// when no agent is bootstrapped OR team_agents.enabled is false for
// the caller's team — same gate the swipe-delegate handler enforces.
// Frontend hides the Bot row in the picker when this is null. The
// per-user TeamMember + TeamMembersResponse shapes live further down
// (where they were originally declared for the predicate editor).
export interface TeamBot {
  agent_id: string
  display_name: string
}

// The conversation status vocabulary — the frontend's ONE copy of it. Every
// status set the UI branches on derives from these three arrays; nothing else
// spells a status name as a bare literal in a list.
//
// How this stays in sync with Go: by hand, plus a test. The names are owned by
// internal/domain/run_status.go, and TestFrontendMirrorsRunStatusVocabulary
// (feature_parity_test.go) parses the arrays below and fails when the two sets
// diverge in EITHER direction — a phase added in Go and not here, or a name
// here that Go never emits. Codegen was considered and rejected: it buys a
// build step and a generated file to keep eleven names honest, and the test
// catches the same drift for the cost of one `go test`.
//
// Both directions matter, and the second is the one that actually bit: the UI
// branched for months on `initializing` and `worktree_created`, neither of
// which the backend has ever emitted, while `awaiting_credentials` — a real
// phase — reached no arm at all.
//
// That test pins these arrays and nothing else, which bought about a week:
// component code doesn't read the arrays, it compares a status against a bare
// literal, and no amount of array-pinning can see a literal in a switch arm.
// So the arrays have a second enforcer — the run-status/no-ghost-run-status
// ESLint rule (frontend/eslint-rules/), which reads the vocabulary out of this
// file and fails the lint on any comparison or `case` that tests a
// conversation status against a name the arrays don't hold.

// CLAIM_PHASES are the setup/parked sub-states of a live executor engagement.
// They arrive in Conversation.Status because display reads coalesce the active
// claim's phase over the conversation's stored status.
export const CLAIM_PHASES = [
  'fetching',
  'cloning',
  'agent_starting',
  'awaiting_credentials',
] as const
export type ClaimPhase = (typeof CLAIM_PHASES)[number]

// TERMINAL_RUN_STATUSES are the states a conversation never leaves: the agent
// concluded, or the infrastructure died. Stopping a run without concluding it
// parks it `open` instead — cancellation is spelled at the task and blueprint
// layers, never as a conversation status.
export const TERMINAL_RUN_STATUSES = ['completed', 'failed'] as const
export type TerminalRunStatus = (typeof TERMINAL_RUN_STATUSES)[number]

// RUN_STATUSES is the full display union: the two derived states (queued and
// running are never stored — they're computed from the claim/queue state),
// the parked state, every claim phase, every terminal.
export const RUN_STATUSES = [
  'queued',
  'running',
  'open',
  ...CLAIM_PHASES,
  ...TERMINAL_RUN_STATUSES,
] as const
export type RunStatus = (typeof RUN_STATUSES)[number]

// RunStatusValue is a conversation status as it arrives over the wire: a plain
// string, deliberately NOT the RunStatus union, so a server emitting a name
// this build predates flows through the lib/runStatus predicates (which
// classify it as unknown) instead of being a compile error at the boundary.
//
// It exists to be a NAME rather than a type constraint. The lint rule reads a
// status comparison two ways — `<expr>.Status` (the PascalCase Conversation
// projection; every other DTO in this file spells its own status lowercase, so
// the case alone separates the curator-turn and blueprint-run vocabularies from
// this one) and any value annotated with this alias. So a helper that takes a
// status second-hand — `runStatusColor(status)`, a tone switch — says so in its
// signature and gets checked like the property access it came from.
export type RunStatusValue = string

// Conversation is the durable agent-context row's display projection —
// served through a handler-side map, so its fields are PascalCase (mirroring
// internal/domain/agent.go's Conversation). One conversation is one delegated
// run today; curator conversations are surfaced through the curator turn DTOs.
export interface Conversation {
  ID: string
  TaskID: string
  // Status is the coalesced display status. RunStatusValue (a string), not the
  // RunStatus union, on purpose — see the alias for why the wire field stays
  // open-world and how the lint rule closes the branching over it.
  Status: RunStatusValue
  Model: string
  StartedAt: string
  // QueuedAt is when the run last entered the queue; ClaimedAt is when the
  // dispatcher last claimed it (work actually began). Together they carry the
  // latest queue episode's dwell — the queue timer — while StartedAt stays the
  // mint stamp and DurationMs stays pure working time. Both absent on legacy
  // rows that predate the queue columns.
  QueuedAt?: string | null
  ClaimedAt?: string | null
  CompletedAt?: string
  TotalCostUSD?: number
  DurationMs?: number
  NumTurns?: number
  StopReason?: string
  ResultSummary: string
  // Outcome is the parsed terminal-envelope outcome
  // (continue|finish|abort), persisted to runs.outcome. Empty/absent for an
  // infra-error run or a step that ended without a recognized conclusion.
  // The blueprint run timeline reads this in place of the old verdict object.
  Outcome?: string
  // OutcomeReason is the "why I stopped" populated only on an abort outcome.
  OutcomeReason?: string
  // FailureKind is the machine-readable failure discriminator for a
  // status='failed' run ('memory_limit' | 'crash' | 'no_result' |
  // 'agent_error'), classified backend-side via errors.Is — never derived
  // from message text. Empty/absent for non-failed runs, legacy failed rows,
  // and failures nothing classified; only 'memory_limit' currently gets
  // distinct rendering (the "Killed: memory limit" badge + the
  // TF_RUN_MEMORY_LIMIT_MB pointer).
  FailureKind?: string
  SessionID?: string
  WorktreePath?: string
  // Derived approval signal. Runs never park for
  // approval; the "needs approval" state is a *view*
  // over the run's unresolved-artifact set. A card surfaces in the approval
  // column whenever has_unresolved_artifacts is true — whether the run is live
  // or terminal — and re-derives back to in-progress (live) / done (terminal)
  // once the last artifact is resolved. These four fields replace the legacy
  // single-kind `pending_kind` / `pending_artifact_id` overlay discriminators.
  //
  // The server emits them only when the answer is *definitive* (the run has no
  // artifacts, or its artifact set was read successfully); on a transient
  // read failure they're OMITTED rather than reported as a misleading false, so
  // consumers treat absence as "unknown" and re-derive on the next refresh.
  has_unresolved_artifacts?: boolean
  // pending_artifact_ids is the set of unresolved approvable artifact ids — every
  // draft PR first (in slice order), then every ready review — the per-item
  // resolve UI lists one card per id. Each is editable (PATCH /api/artifacts/{id})
  // and approvable (POST /api/artifacts/{id}/approve) / dismissable
  // (POST /api/artifacts/{id}/dismiss). [] (not undefined) when nothing is
  // unresolved but the set was read; undefined under the transient-failure guard.
  pending_artifact_ids?: string[]
  // Per-kind counts of the unresolved set, for count-aware labels ("N ready",
  // "Review N items →") and the resolve-all confirmation copy. unresolved_pr_count
  // draft PRs + unresolved_review_count ready reviews === pending_artifact_ids.length.
  unresolved_pr_count?: number
  unresolved_review_count?: number
  // artifact_count is the number of artifacts this run produced (TFAC-465's
  // runResponse projection — branch / PR / review / issue / comment, the
  // primary gating one included). The Board card shows it as a footer
  // affordance without a per-card fetch; 0 / undefined hides the affordance.
  artifact_count?: number
  // actor_agent_id / actor_agent_name identify the bot that executed this run
  // (runs.actor_agent_id), denormalized from agents.display_name
  // via a JOIN on the run read projections. The card renders "Ran as: {name}" when
  // a name is present; both are absent/empty for a run with no actor (spawned before
  // agent bootstrap, or after the agent row was deleted).
  actor_agent_id?: string
  actor_agent_name?: string
  blueprint_run_id?: string
  blueprint_step_index?: number | null
  // blueprint_step_count is the length of the owning blueprint run's frozen
  // step plan — what turns blueprint_step_index into a position ("step 2 of
  // 4") and says whether a completed step is the chain's last. 0 when the
  // server could not resolve the plan (a manual blueprint run is creator-scoped
  // under RLS, so a teammate reads 0); lib/runStatus treats that as unknown and
  // falls back to the unqualified reading. Every delegated run belongs to a
  // blueprint, so 1 — not 0 — is the plain single-prompt run.
  blueprint_step_count?: number
  // Token rollups: the SUM over this conversation's messages, derived by the
  // same run read that carries TotalCostUSD / DurationMs / NumTurns. The
  // authoritative numbers — the same ones the usage dashboard reports — so a
  // surface reads them here rather than walking the transcript. 0 for a run
  // that never streamed a usage-bearing message; useRunDetail folds live
  // per-message deltas on top between refetches of the run row, exactly as it
  // does for cost.
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_creation_tokens?: number
  // resumable is the server's answer to "will a follow-up be accepted?" — the
  // three-part backend predicate (status, workspace survival, blueprint
  // drivability) of which the client can see exactly one part. The composer
  // gates on it: a stopped run whose workspace never made it, was reaped by
  // retention, or predates the snapshot work looks identically resumable from
  // Status alone and answers every message with a 409/410.
  //
  // Detail read only (GET /api/agent/conversations/{id}) and only for a run
  // that is neither active nor failed — the board shows no composer, an active
  // run is steered through its live process, and a failed one has no workspace.
  // ABSENT therefore means "the server didn't answer", not false: consumers
  // fall back to the status-only reading, which is correct for the runs that
  // skip it. It is a read, not a promise — the send's own 409/410 stays the
  // enforcement.
  resumable?: boolean
  // resume_blocked_reason names the rung that refused, present only when
  // resumable is false: 'workspace_expired' | 'blueprint_concluded' |
  // 'session_missing' | 'worktree_missing' | 'model_missing' | 'not_steerable'
  // (internal/delegate's ResumeBlocked* constants). Open-world by design — an
  // unrecognized reason renders as the generic "can't be resumed" copy.
  resume_blocked_reason?: string
}

// ArtifactKind is the closed set of artifact discriminators the backend emits
// (internal/domain/artifact.go). Kept a strict union — not widened with
// `| string`, which would collapse the whole type to `string` and erase the
// narrowing. A server value outside this set is handled defensively at the
// render layer (a fallback icon/label), it just isn't part of the documented
// contract.
export type ArtifactKind = 'branch' | 'pull_request' | 'review' | 'issue' | 'comment' | 'message'

// Artifact mirrors the GET /api/agent/conversations/{id}/artifacts wire shape
// (internal/server/agent.go artifactJSON, TFAC-465). One row per real external
// object a run produced. `state` is meaningful only read with `kind` (see
// internal/domain/artifact.go — 'pending' aliases across kinds). `details` is
// the parsed kind-specific payload (or null when absent/unparseable).
export interface Artifact {
  id: string
  kind: ArtifactKind
  provider: string
  state: string
  target: string
  external_id: string
  url: string
  details: unknown
  created_at: string
}

// ActivityArtifact is the activity feed's Objects-lens row shape (TFAC-483): an
// Artifact plus the owning team's id + name. The team fields are populated ONLY
// by the org-wide feed (GET /api/usage/org/activity?view=objects) so a cross-team
// row shows which team's bot acted; the team-scoped feed omits them (it's already
// one team). Mirrors internal/server activityArtifactJSON.
export interface ActivityArtifact extends Artifact {
  team_id?: string
  team_name?: string
}

// ActivityAction is the activity feed's Actions-lens row shape (TFAC-483): one
// row of the append-only external-action audit log — a single external WRITE TF
// performed under an org credential. `action` is the discriminator (the backend
// domain.Action* consts); `from_state`/`to_state` carry a transition; `details`
// is the parsed per-action payload (or absent). team_id/team_name/actor_name are
// populated ONLY by the org-wide feed (GET …/org/activity?view=actions) so a
// cross-team row shows which team acted and who authorized it; the team feed
// omits them. Mirrors internal/server actionJSON.
export interface ActivityAction {
  id: string
  provider: string
  action: string
  target: string
  external_id?: string
  url?: string
  from_state?: string
  to_state?: string
  conversation_id?: string
  actor_user_id?: string
  credential: string
  details?: unknown
  occurred_at: string
  team_id?: string
  team_name?: string
  actor_name?: string
}

// Message is the single snake_case transcript-row DTO shared by every
// surface (delegated runs and curator chat), mirroring domain.MessageDTO
// (internal/domain/agent.go). conversation_id links the row to its owning
// conversation; curator groups these into turns client-side.
export interface Message {
  id: number
  conversation_id: string
  role: string
  content: string
  subtype: string
  tool_calls?: ToolCall[]
  tool_call_id?: string
  is_error?: boolean
  metadata?: Record<string, unknown>
  model?: string
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_creation_tokens?: number
  // cost_usd is the dollars settled at this row — absent when the row is not a
  // settlement row, 0 when it is and cost nothing. A runtime that stamps as it
  // streams turns these into a live spend signal: useRunDetail folds each
  // stamped row into the displayed run total between refetches of the
  // conversation's authoritative SUM.
  cost_usd?: number
  created_at: string
  // duration_ms is how long THIS row's own work took — an assistant row from
  // the request going out to the message completing (reasoning included, so
  // "thought for Ns" comes off the row that did the thinking), a tool row from
  // dispatch to result. Work only: a permission-gated call reads as the time
  // it ran, never the time it stood waiting for someone to approve it.
  // Absent means nobody measured it (every row written
  // before the runtime stamped timing, and every non-agent role), which is not
  // the same as 0 — so read it with `!= null`, never `?? 0`. Never derive a
  // duration by subtracting a neighbour's created_at: streaming, paging, and
  // compaction each break that in their own way.
  duration_ms?: number
  // reasoning/content_blocks mirror domain.MessageDTO's fields of the same
  // name — absent on messages that carry neither. reasoning rides the
  // assistant message it belongs to rather than arriving as a separate
  // subtype:"thinking" row; content_blocks holds non-text content (e.g. an
  // image tool result) the flat content string can't carry.
  reasoning?: ReasoningDetail[]
  content_blocks?: ContentBlock[]
}

export interface ToolCall {
  id: string
  name: string
  input: Record<string, unknown>
}

// ReasoningDetail/ContentBlock* mirror the Go domain types of the same name
// (internal/domain/agent.go). Every wire message DTO is snake_case.
export interface ReasoningDetail {
  index: number
  type: string
  text?: string
  signature?: string
  data?: string
}

export type ContentBlockType = 'text' | 'image_url' | 'file'

export interface ContentBlock {
  type: ContentBlockType
  text?: string
  image_url?: ContentImageURL
  file?: ContentFile
}

export interface ContentImageURL {
  url?: string
  file_id?: string
  detail?: string
}

export interface ContentFile {
  file_data?: string
  file_url?: string
  file_id?: string
  filename?: string
  file_type?: string
}

// CuratorRequest mirrors the Go domain type in internal/domain/curator.go —
// one curator chat turn (a user message plus the claim that executed it). Its
// agent-side rows are the shared snake_case Message DTO.
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
  messages: Message[]
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

export interface Prompt {
  id: string
  name: string
  body: string
  source: string
  // Per-prompt model override; '' = inherit the team default at dispatch.
  // Optional on the wire shape some callers build, but `/api/prompts`
  // serializes it (domain.Prompt.Model) — the picker's detail pane reads it
  // to show a per-step model chip.
  model?: string
  usage_count: number
  created_at: string
  updated_at: string
}

// Blueprint is the unified successor to prompt-chains. A blueprint is an
// ordered list of steps; a single-step blueprint is what used to be a "leaf"
// prompt. Multi-step blueprints replace the former "chain" prompts.
export interface Blueprint {
  id: string
  name: string
  // source is 'system' for shipped/template-seeded blueprints, 'user' for
  // authored ones. Optional because not every Blueprint-shaped response sets it.
  source?: string
  team_id?: string
  // How many runs have used this blueprint. `/api/blueprints` serializes it
  // (domain.Blueprint.UsageCount); the picker surfaces it in the detail pane.
  // Optional because not every Blueprint-shaped response carries it.
  usage_count?: number
  created_at: string
  updated_at: string
}

export interface BlueprintStep {
  blueprint_id: string
  step_index: number
  step_prompt_id: string
  // Optional: the step prompt's name. Only a blueprint run's frozen step_plan
  // snapshots it — the live blueprint_steps row the /steps editor reads holds a
  // prompt id and no name — so render it when present and fall back to the
  // brief.
  name?: string
  brief: string
  // Optional: a step rebuilt from a blueprint run's frozen step_plan has no
  // live blueprint_steps row, so the run projection omits created_at (the
  // /steps editor reads still carry it).
  created_at?: string
}

export interface BlueprintRunStepView {
  step: BlueprintStep
  run?: Conversation
}

export interface BlueprintRunResponse {
  blueprint_run: BlueprintRun
  steps: BlueprintRunStepView[]
}

export interface BlueprintRun {
  id: string
  blueprint_id: string
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
  // supports_watch: whether the applies_to_unowned ("watch") toggle is
  // meaningful for this event (TFAC-519). True only for owner-ladder events;
  // the rule/trigger editors hide the toggle when false (it would be inert).
  supports_watch: boolean
}

// Event handlers — unified successor to the former TaskRule
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
  // applies_to_unowned: the explicit "watch" scope flag (TFAC-517). When true,
  // the rule reaches entities the team doesn't own (e.g. PRs/issues authored by
  // anyone), surfacing them to the team's board. Default off — visibility
  // otherwise rides ownership.
  applies_to_unowned: boolean
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
  blueprint_id: string
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
  /** Whether self-service users may create their own org (the inverse
   *  of the instance's TF_PREVENT_ORG_CREATION). Drives the onboarding
   *  entry: when false, the "create your org" affordance is disabled and
   *  the page shows the invite-only "ask your admin" state. Always sent
   *  by the server (both modes) — true in local mode (N=1, never renders
   *  onboarding). */
  org_creation_enabled: boolean
  /** Whether this identity is a deployment operator (TFAC-589) — the org-less
   *  flag managed by `triagefactory operator`. Composes with the FeatureFleet
   *  entitlement to gate the Fleet console nav item + page. Always sent (both
   *  modes); true in local mode (the single user is the implicit operator). */
  is_operator: boolean
}

/** One linked login identity for the signed-in principal, from
 *  GET /api/me/identities. A principal holds N identities — a GitHub login
 *  plus N SSO providers — that resolve to one account via verified-email
 *  linking at login. The opaque GoTrue bridge ids (auth_user_id,
 *  provider_subject) are never sent: no user value, internal keys. */
export interface LoginMethod {
  /** 'github' | 'saml' in multi mode; 'local' for the N=1 local stub. */
  provider: string
  /** The identity's email. Absent for the local stub (no email concept). */
  email?: string
  email_verified: boolean
  /** RFC3339 timestamp the identity was first linked (its created_at).
   *  Absent for the local stub. */
  linked_at?: string
  /** True for the identity backing the current browser session — the login
   *  method you're signed in with right now. */
  current: boolean
}

/** GET /api/me/identities — the "Login methods" view. Personal read scoped to
 *  the session principal; works in both modes (local returns one synthetic
 *  row). */
export interface IdentitiesResponse {
  methods: LoginMethod[]
}

/** GET /api/orgs/{org}/github/identity — the onboarding gate's status read.
 *  `connected` is the single bit the gate blocks on (a host-verified GitHub
 *  identity exists for the active org's host). An absent binding is
 *  connected=false, which the runtime tolerates as NULL — the gate just
 *  asks the user to bind one before entering the app. */
export interface GitHubIdentityStatus {
  connected: boolean
  /** The bound login, when connected. */
  login?: string
  /** The org's GitHub host the binding is keyed against. */
  host: string
  /** Whether Connect is offerable — true once the org has a registered
   *  GitHub App (Connect reuses its client_id). When false, the gate tells
   *  the user their admin must finish GitHub setup. */
  connect_available: boolean
}

/** GET /api/orgs/{org}/jira/identity — the Jira sibling of GitHubIdentityStatus.
 *  Unlike GitHub (identity only), Jira's user level holds *access*, so
 *  `connected` reflects a STORED per-user credential, not just an identity row.
 *  An absent credential is connected=false (the gate asks the user to bind one
 *  before entering the app). */
export interface JiraIdentityStatus {
  connected: boolean
  /** The bound account (display name), when connected — the Jira analog of
   *  GitHub's @login. */
  account?: string
  /** The org's Jira host the credential is keyed against. */
  host: string
  /** Whether one-click Connect is offerable — true when an Atlassian OAuth
   *  app resolves for the org. False (DC, or no OAuth app configured) means
   *  the surfaces offer only the token path. */
  connect_available: boolean
  /** The org's Jira backend ("cloud" / "data_center"), so the paste surfaces
   *  render the right fields — a Cloud org binds an email + API token, a Data
   *  Center org a single PAT. Empty/absent when no Jira host is configured. */
  deployment?: 'cloud' | 'data_center'
}

// ──────────────────────────────────────────────────────────────────────
// Org invites (TFAC-418, consuming the TFAC-416 backend). Multi-mode only.
// The admin surfaces (create/list/revoke) are session-org-scoped — the
// backend resolves the org from the session's active org, so the frontend
// calls the literal /api/invites paths WITHOUT an org prefix (same as
// /api/teams), never via apiClient's `org` option.
// ──────────────────────────────────────────────────────────────────────

/** One pending invite from GET /api/invites — the rows the org People
 *  roster renders as muted "ghost" rows beneath real members. `created_at`
 *  drives the "invited {relative time}" label; `expires_at` is the 7-day TTL. */
export interface PendingInvite {
  id: string
  email: string
  role: string
  /** Set when the invite targets a specific team; omitted for an org-only
   *  invite (member of the org, on zero teams). */
  target_team_id?: string
  invited_by?: string
  created_at: string
  expires_at: string
}

/** POST /api/invites 201 response. `accept_url` is the single-use link shown
 *  ONCE (the raw token is never returned again — copy/paste IS the delivery
 *  in v1, no SMTP). */
export interface CreateInviteResponse {
  id: string
  accept_url: string
  expires_at: string
}

/** The redeemability of an invite token, from GET /api/invites/preview.
 *  Only `valid` is acceptable; the rest are terminal states the accept page
 *  renders as a dead end. */
export type InvitePreviewStatus = 'valid' | 'expired' | 'revoked' | 'accepted' | 'not_found'

/** GET /api/invites/preview?token=… response (unauthenticated). The token is
 *  the bearer secret, so org name + role to its holder is fine; everything
 *  but `status` is omitted for the terminal states. */
export interface InvitePreview {
  org_name?: string
  role?: string
  invited_by_name?: string
  expires_at?: string
  status: InvitePreviewStatus
}

/** POST /api/invites/accept 200 response — the org the recipient just joined
 *  (the backend has already pointed their session at it). */
export interface AcceptInviteResponse {
  org_id: string
}

/* ── SSO (multi-org SAML via GoTrue, epic TFAC-422) ──────────────────────────
 *  Wire shapes for the per-org "Configure SSO" admin surface (TFAC-429), which
 *  consumes the connection (TFAC-424) and domain (TFAC-428) endpoints. Multi-
 *  mode only; admin-gated. GoTrue owns authN + the provider registry, TF owns
 *  the org↔provider binding — `provider_id` is the bridge, and these carry no
 *  IdP secrets (the signing material lives in GoTrue). */

/** The IdP protocol of a connection. SAML today; OIDC is a sibling for later
 *  (picked per the customer's IdP). */
export type SSOConnectionKind = 'saml' | 'oidc'

/** One registered IdP connection, from the `connection` field of
 *  GET/POST/PATCH /api/sso/connection (null until one is registered). A freshly
 *  registered connection is created enabled; disabling pauses SSO sign-in
 *  without removing the binding. */
export interface SSOConnection {
  id: string
  kind: SSOConnectionKind
  /** GoTrue sso_providers.id (UUID); opaque to TF, globally unique. */
  provider_id: string
  /** The org_role JIT-provisioned users get: 'admin' | 'member' (never owner). */
  default_role: string
  enabled: boolean
  /** "Require SSO" state. When true, a non-SSO (GitHub) login on one
   *  of this connection's verified domains is rejected unless the principal is
   *  break-glass. Separate axis from `enabled` ("allow" vs "require"). */
  enforced: boolean
  /** Whether the connection has passed a verify-before-enforce Test. The
   *  enforce toggle is gated on this (plus `enabled` + a verified domain). */
  tested: boolean
  created_at: string
  updated_at: string
}

/** GET/POST/PATCH /api/sso/connection response. The SP values are always
 *  present — even with no connection — because they're a pure function of the
 *  deployment's public URL, so the operator can configure the IdP side first.
 *  `entity_id` → the Entra "Identifier"; `acs_url` → the Entra "Reply URL". */
export interface SSOConnectionResponse {
  connection: SSOConnection | null
  entity_id: string
  acs_url: string
}

/** A claimed domain's lifecycle: inert (`pending`) until DNS-TXT verification
 *  flips it to `verified`. Only a verified domain routes sign-in, and a verified
 *  domain belongs to ≤1 org deployment-wide. */
export type SSODomainStatus = 'pending' | 'verified'

/** One claimed email domain, from GET/POST /api/sso/domains and the verify
 *  endpoint. `txt_host`/`txt_value` are the exact DNS record to publish
 *  (`<txt_host>  TXT  <txt_value>`) — public by design (it proves zone
 *  control), so not a secret. `verified_at` rides along once stamped. */
export interface SSODomain {
  id: string
  domain: string
  txt_host: string
  txt_value: string
  status: SSODomainStatus
  verified_at?: string
}

/** One break-glass principal, from GET/POST /api/sso/break-glass — a
 *  principal allowed to keep signing in via GitHub under SSO enforcement (the
 *  recovery path). The owner is the default, seeded when enforcement is enabled. */
export interface BreakGlassPrincipal {
  user_id: string
  display_name?: string
  email?: string
  created_at: string
}

/** GET /api/team/members row. Backs Variant B's searchable multi-select.
 *  Local mode returns a single-entry array containing the synthetic
 *  LocalDefaultUserID; multi mode returns the active
 *  user's team roster. */
export interface TeamMember {
  user_id: string
  display_name: string
  github_username: string | null
  /** Atlassian account ID. Null when this member hasn't
   *  connected Jira yet. */
  jira_account_id: string | null
  is_current_user: boolean
}

export interface TeamMembersResponse {
  members: TeamMember[]
  // Bot entry, populated when the caller's team has an
  // enabled agent (otherwise null). Same gate as swipe-delegate.
  bot: TeamBot | null
}

/** One team's tracking claim on a Slack channel (TFAC-543) — the tracked_by
 *  entry on a channel row. Name + role only, never activity/config (the
 *  ratified cross-team disclosure line — see ee/slack's trackerView). */
export interface SlackChannelTracker {
  team_id: string
  team_name: string
  is_primary: boolean
}

/** One merged row of GET/PUT /api/slack/teams/{team_id}/channels — the union
 *  of this team's tracked rows, the org's sighting registry, and live Slack
 *  candidates, deduped by channel_id (TFAC-543). `name` is empty when only a
 *  bare channel_id is known (no live/sighting resolution yet) — render the
 *  raw id rather than inventing a name. */
export interface SlackChannelView {
  channel_id: string
  name: string
  workspace_id: string
  is_private: boolean
  bot_is_member: boolean
  tracked: boolean
  is_primary: boolean
  tracked_by: SlackChannelTracker[]
  last_mention_at?: string
  source: 'tracked' | 'sighting' | 'slack'
}

/** One entry in a Slack channels response's `warnings` — a tagged union: a
 *  process-level degradation (`code`, e.g. "slack_unreachable" |
 *  "slack_channels_truncated") on GET, or a per-channel PUT auto-join
 *  outcome (`channel_id` + `reason`, e.g. "invite_required" | "join_failed"). */
export interface SlackChannelsWarning {
  code?: string
  channel_id?: string
  reason?: string
}

/** GET/PUT /api/slack/teams/{team_id}/channels response. PUT's response is
 *  the post-change GET shape plus any auto-join warnings appended — callers
 *  refresh their rows straight from it rather than re-fetching. */
export interface SlackChannelsResponse {
  role: string
  warnings: SlackChannelsWarning[]
  channels: SlackChannelView[]
}

/** Project.visibility — see domain.Project.Visibility. Multi-mode only:
 *  local mode's projects are always "team" and never expose a control for
 *  this. */
export type ProjectVisibility = 'private' | 'team' | 'org'

export interface Project {
  id: string
  name: string
  description: string
  /** The team that owns this project (domain.Project.TeamID). The
   *  pinned-repos editor sources its options from this team's tracked
   *  set, since the PATCH validator only accepts repos this team tracks.
   *  Empty for a private/org project created without one (see
   *  visibility) — pinned repos and tracker keys aren't available then. */
  team_id: string
  /** Who can read/write this project: "private" (creator only), "team"
   *  (the owning team's write-members), or "org" (every org member
   *  reads; only org admins create/downgrade into it). The
   *  projects_{select,insert,update} RLS policies are the actual
   *  enforcement — the UI's job is to gray out choices the viewer
   *  doesn't have, not to be the source of truth. */
  visibility: ProjectVisibility
  /** The user who created the project (domain.Project.CreatorUserID).
   *  Only the creator may set visibility to "private" (mirrors the
   *  projects_update RLS policy's private branch) — the edit surface
   *  compares this against the viewer's own id to gray that option out
   *  for anyone else. */
  creator_user_id: string
  pinned_repos: string[]
  jira_project_key: string
  linear_project_key: string
  /** Per-project Curator spec-authorship skill. Empty string =
   *  use the seeded `system-ticket-spec` default. The Curator dispatch
   *  materializes whichever prompt this points at as a literal Claude
   *  Code skill at `<cwd>/.claude/skills/ticket-spec/SKILL.md` on every
   *  turn — changes apply immediately without a session reset. */
  spec_authorship_blueprint_id: string
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
  // Non-fatal gaps: content that exists but couldn't be included (e.g.
  // a session transcript unreadable by the server process). The bundle
  // still exports; the user should know it ships without this.
  warnings?: string[]
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
  /** True if any run on this entity is `open` (a turn ended without a
   *  conclusion). Drives the idle badge on the runs-tray chip so a user
   *  scanning the factory can spot open runs without opening each card. */
  has_open_run?: boolean
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
    run: Conversation
    task: Task
    mine: boolean
  }>
}

export interface FactorySnapshot {
  stations: Record<string, FactoryStation>
  entities: FactoryEntity[]
}

export type WSEvent =
  // One transcript row for a conversation — delegated run or curator turn
  // alike (the two former agent_message / curator_message events converged).
  // conversation_id is set for delegated runs; project_id is set for curator
  // turns (the curator surface filters by project and appends to its active
  // turn). data is the shared snake_case Message DTO.
  | { type: 'message'; conversation_id?: string; project_id?: string; data: Message }
  // Conversation lifecycle/status change (the former agent_run_update +
  // curator_request_update). A delegated run carries conversation_id and a
  // coalesced display status (fetching/cloning/agent_starting/
  // awaiting_credentials/running/terminal) plus failure_kind on a failure; a
  // curator turn carries project_id and { request_id, status }. failure_kind
  // rides along only when status === 'failed' AND the backend classified the
  // cause (domain.RunFailureKind); absent === generic failure.
  | {
      type: 'conversation_update'
      conversation_id?: string
      project_id?: string
      data: {
        status?: string
        failure_kind?: string
        request_id?: string
        // resumable rides the parked status when a run's workspace snapshot
        // lands after the park was already announced (a cross-pod stop parks
        // from control seconds before the executor writes the blob) — the
        // moment a follow-up becomes possible, which no status change marks.
        // The status repeats what the row already has, so consumers must merge
        // it idempotently: a second `open` is not a transition.
        resumable?: boolean
      }
    }
  // Artifact reconciliation (TFAC-464): an artifact a conversation produced
  // changed state on GitHub (PR merged/closed, branch deleted, review
  // submitted). The conversation's own status is unchanged — consumers refetch
  // to pick up its artifact-derived surface (pending kind / approval card).
  // Distinct from conversation_update precisely so it never feeds the Board's
  // optimistic status write.
  | {
      type: 'artifact_updated'
      conversation_id: string
      data: { artifact_id: string; state: string }
    }
  | {
      // P3 steering: a conversation surfaced a tool-permission prompt
      // (canUseTool), answered via
      // POST /api/agent/conversations/{conversationID}/permissions/{toolCallID}.
      // tool_call_id is the tool_use id of the gated call — the same id the
      // assistant row's tool_calls and the tool result carry. timeout_ms is the
      // prompt's server-side deadline (relative); the dock derives its dismiss
      // TTL from it. title/display_name/description are the SDK's own prompt
      // copy, present only when it rendered any.
      type: 'permission_request'
      conversation_id: string
      // Just the id: the prompt itself is read from
      // GET /api/agent/conversations/{id}/permissions, so this frame is a
      // refetch trigger like artifact_updated rather than the only path to the
      // state. That is what makes a refresh, a second tab, and a cold load able
      // to reconstruct a prompt the frame fired once for and never repeated.
      data: { tool_call_id: string }
    }
  | {
      // A pending permission prompt reached a terminal resolution (answered by
      // someone, or timed out) — broadcast so every surface showing it (board +
      // run-detail, or two board tabs) drops it promptly instead of waiting for
      // its own client TTL. The client TTL stays as a backstop.
      type: 'permission_resolved'
      conversation_id: string
      data: { tool_call_id: string }
    }
  | {
      // The executor syncer (multi mode) always carries a `pending` field: the
      // batch's in-flight filenames on start — the panel renders ghost rows for
      // them so a large-video upload reads as progress — and an empty array on
      // completion, which is how the panel tells a sync signal apart from the
      // control pod's own upload/delete broadcast (that, and local mode, send
      // data: null and only trigger a refetch, never touching ghost rows).
      type: 'project_knowledge_updated'
      project_id: string
      data: { pending?: string[] } | null
    }
  | {
      type: 'entities_assigned_to_project'
      project_id: string
      data: { entity_ids: string[] }
    }
  | {
      // The requesting user's live curator conversation was archived (reset).
      // Scoped to the resetting user server-side; carries the archived
      // conversation id so a client that already began a fresh conversation
      // ignores the stale reset instead of wiping it.
      type: 'conversation_reset'
      project_id: string
      conversation_id?: string
      data: { conversation_id: string }
    }
  | { type: 'event'; data: DomainEvent }
  | { type: 'tasks_updated'; data: Record<string, never> }
  | {
      // Claim-axis change. Exactly one of the two ID fields
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
      // One sparse-diff event for the repositories table (mirrors Go's
      // repoevent.Update allowlist): only the fields the backend just
      // wrote are present, and consumers merge them into the row keyed
      // by id — never overwrite, so a clone-status diff can't blank the
      // AI profile text and vice versa.
      type: 'repository_updated'
      data: {
        id: string
        has_readme?: boolean
        has_claude_md?: boolean
        has_agents_md?: boolean
        profile_text?: string
        clone_status?: 'ok' | 'failed' | 'pending'
        clone_error?: string
        clone_error_kind?: 'ssh' | 'other'
      }
    }
  | { type: 'toast'; data: ToastPayload }

// ──────────────────────────────────────────────────────────────────────
// Usage dashboard — spend layer (TFAC-479, consuming the TFAC-478 backend).
// Three role-gated, session-org-scoped reads over the llm_spend view. The FE
// calls the literal /api/usage/* paths WITHOUT an org prefix — the backend
// resolves the org from the session, same as /api/dashboard/*. snake_case
// fields mirror the Go response structs in internal/server/usage_handler.go.
// `cost` is real USD (SDK token counts × list price; see the TFAC-449 epic),
// not an estimate.
// ──────────────────────────────────────────────────────────────────────

/** One spend category's cost + token totals over the window. `category` is one
 *  of the domain.SpendCategory* values — 'manual' | 'autonomous' | 'curator' |
 *  'system_overhead' — rendered via the categoryLabel map in Usage.tsx. */
export interface UsageCategoryBucket {
  category: string
  cost: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_creation_tokens: number
}

/** One model's summed cost, highest-first. Curator turns carry a NULL model and
 *  are excluded server-side, so by_model is a per-model slice, not a total. */
export interface UsageModelBucket {
  model: string
  cost: number
}

/** One UTC calendar day's total cost (date as YYYY-MM-DD), oldest-first — the
 *  time series the "over time" area chart plots. */
export interface UsageDayBucket {
  date: string
  cost: number
}

/** One (UTC day, model) cell — long-format, oldest-first then by model. The FE
 *  pivots it into a stacked-by-model area over time. Curator rows (NULL model)
 *  are excluded (their share is still in by_day's per-day total). */
export interface UsageDayModelBucket {
  date: string
  model: string
  cost: number
}

/** One human creator's summed cost (their manual runs + curator turns). */
export interface UsageUserBucket {
  user_id: string
  display_name: string
  /** OAuth-captured avatar URL; omitted when unset (e.g. local mode), where the
   *  roster falls back to a monogram. */
  avatar_url?: string
  cost: number
}

/** One firing trigger's summed cost across autonomous runs. `rule_name` resolves
 *  to the trigger's blueprint name, falling back to '' (the UI then shows the
 *  trigger id). */
export interface UsageRuleBucket {
  trigger_id: string
  rule_name: string
  cost: number
}

/** One team's summed cost across team-attributed rows (org rollup only). Per-team
 *  caps are NOT here — the governance cap editor reads the full team list from
 *  /api/usage/org/team-caps (UsageTeamCap), so an idle team absent from this spend
 *  rollup can still be capped (TFAC-482). */
export interface UsageTeamBucket {
  team_id: string
  team_name: string
  cost: number
}

/** One category of org-level spend — the NULL-team rows (curator on non-team
 *  projects + system overhead) that aren't attributable to any one team. */
export interface UsageOrgLevelBucket {
  category: string
  cost: number
}

/** GET /api/usage/me — the caller's own spend (any org member). */
export interface UsageMeResponse {
  total_cost_usd: number
  by_category: UsageCategoryBucket[]
  by_model: UsageModelBucket[]
  by_day: UsageDayBucket[]
  by_day_model?: UsageDayModelBucket[]
}

/** GET /api/usage/teams/{id} — one team's breakdown (team admin only; an org
 *  admin who isn't a team admin gets a 403 and sees cross-team numbers in the
 *  org rollup instead). */
export interface UsageTeamResponse {
  team_id: string
  team_name: string
  total_cost_usd: number
  by_category: UsageCategoryBucket[]
  by_user: UsageUserBucket[]
  by_rule: UsageRuleBucket[]
  by_model: UsageModelBucket[]
  by_day: UsageDayBucket[]
  by_day_model?: UsageDayModelBucket[]
}

/** GET /api/usage/org — the org rollup (org admin only). Partition invariant
 *  (from the backend): total_cost_usd === sum(by_team) + sum(org_level); by_user
 *  and by_category slice the same total on different axes. */
export interface UsageOrgResponse {
  total_cost_usd: number
  by_team: UsageTeamBucket[]
  by_user: UsageUserBucket[]
  org_level: UsageOrgLevelBucket[]
  by_category: UsageCategoryBucket[]
  by_model: UsageModelBucket[]
  by_day: UsageDayBucket[]
  by_day_model?: UsageDayModelBucket[]
  /** Present ONLY in local mode (N=1) — the org rollup omits per-rule detail in
   *  multi mode (it stays with the owning team). Lets the local console read
   *  everything in one request. */
  by_rule?: UsageRuleBucket[]
}

/** One team in GET /api/usage/org/team-caps — its id, name, and per-team daily
 *  spend cap (TFAC-482; null = no cap). The governance cap editor lists EVERY
 *  active team this way (not just those with spend), so an idle team can be
 *  pre-capped; window spend is looked up separately from the org rollup's by_team. */
export interface UsageTeamCap {
  team_id: string
  team_name: string
  cap: number | null
}

/** GET /api/usage/org/team-caps — every active team + its cap (org admin +
 *  governance only; 404 unlicensed). */
export interface UsageTeamCapsResponse {
  teams: UsageTeamCap[]
}

/** One row of the EE access & credential change-log (GET
 *  /api/usage/org/access-log, TFAC-484). `action_label` is the server-rendered
 *  human predicate ("changed Alice from member to admin") shown after the actor +
 *  timestamp; the actor/target/team names are pre-resolved ("" when a
 *  since-removed user/team no longer resolves). `action` is the raw discriminator
 *  (used FE-side only to tone membership vs credential rows). */
export interface AccessChangeRow {
  id: string
  action: string
  action_label: string
  actor_name: string
  target_name?: string
  team_name?: string
  /** Raw captured payload, passed through for power users; the FE renders the
   *  label, not this. The wire value is a json.RawMessage — arbitrary JSON, not
   *  necessarily an object — so it's typed `unknown` (narrow before use). Omitted
   *  when the row carried no (valid) detail. */
  detail_json?: unknown
  created_at: string
}

/** GET /api/usage/org/access-log — one page of the org-admin EE audit viewer
 *  (org admin + governance entitlement; 404 unlicensed). Newest-first; paginate
 *  via limit/offset and `has_more`. The `category` query narrows to membership vs
 *  credential vs policy (SSO connection, enforcement, domains, break-glass)
 *  changes. */
export interface AccessLogResponse {
  items: AccessChangeRow[]
  limit: number
  offset: number
  has_more: boolean
}

// --- Fleet console (TFAC-589) — mirrors ee/fleet DTOs ---

export interface FleetTotals {
  instances: number
  executors: number
  control: number
  draining: number
  gated: number
  stale: number
  capacity_max: number
  active_runs: number
}

export interface FleetQueueSummary {
  depth: number
  oldest_wait_seconds: number
  wait_p50_ms?: number
  wait_p95_ms?: number
}

export interface FleetFailureKindRate {
  kind: string
  count: number
}

export interface FleetRunsSummary {
  window_hours: number
  total: number
  active: number
  completed: number
  failed: number
  duration_p50_ms?: number
  duration_p95_ms?: number
  failure_kinds: FleetFailureKindRate[]
}

export interface FleetVersionSkew {
  version: string
  count: number
}

export interface FleetSpendSummary {
  window_hours: number
  total_usd: number
}

export interface FleetOverview {
  generated_at: string
  stale_after_seconds: number
  totals: FleetTotals
  queue: FleetQueueSummary
  runs: FleetRunsSummary
  versions: FleetVersionSkew[]
  spend?: FleetSpendSummary
}

export interface FleetInstance {
  id: string
  role: string
  version: string
  boot_epoch: number
  started_at: string
  last_heartbeat_at: string
  heartbeat_age_seconds: number
  stale: boolean
  draining: boolean
  dispatch_gated?: boolean
  max_runs?: number
  active_runs?: number
  mem_total_mb?: number
  mem_available_mb?: number
  cpu_pct?: number
  load1?: number
  claims_last_sample?: number
  spawn_p50_ms?: number
}

export interface FleetInstances {
  generated_at: string
  stale_after_seconds: number
  instances: FleetInstance[]
}

export interface FleetSample {
  instance_id: string
  at: string
  active_runs?: number
  queued_visible?: number
  mem_available_mb?: number
  cpu_pct?: number
  load1?: number
  claims?: number
  spawn_p50_ms?: number
  oom_kills?: number
}

export interface FleetTimeseries {
  generated_at: string
  window_hours: number
  samples: FleetSample[]
}

// FleetBacklog mirrors the EE console's GET /api/fleet/backlog (TFAC-589): the
// operator wait-latency lens — fleet-wide queue depth + the single oldest wait,
// and per-org shares by pending count and each org's own oldest wait. Distinct
// from core's /api/fleet/queue (the org-facing per-org cap read-out).
export interface FleetBacklogOrgShare {
  org_id: string
  count: number
  oldest_wait_seconds: number
}

export interface FleetBacklog {
  generated_at: string
  depth: number
  oldest_wait_seconds: number
  by_org: FleetBacklogOrgShare[]
}

// FleetSandboxClaim mirrors the EE console's
// GET /api/fleet/instances/{id}/sandboxes — one executor engagement and what
// its sandbox actually cost, the operator lens the whole-host instance_stats
// samples structurally cannot give.
//
// Every measurement is optional because absent means NOT MEASURED, never
// measured-zero: a local-mode claim had no sandbox, a pre-5.19 kernel has no
// memory.peak, a crashed teardown recorded neither. Render a dash, not a 0.
export interface FleetSandboxClaim {
  id: string
  conversation_id: string
  org_id: string
  claimed_at: string
  released_at?: string
  /** Unreleased — still holding a slot, and its series is still growing. */
  live: boolean
  /** Wall clock: claimed → released, or claimed → now while live. */
  duration_seconds: number
  peak_mem_mb?: number
  cpu_usec?: number
  /** The driven conversation's status — the same vocabulary Conversation.Status
   *  carries, so branch on it through a RunStatusValue-annotated helper rather
   *  than inline literals. */
  status?: RunStatusValue
  failure_kind?: string
  /** How the ENGAGEMENT ended (completed | failed | cancelled | requeued |
   *  parked | reaped) — a claim vocabulary of its own, not a run status. */
  outcome?: string
}

export interface FleetSandboxes {
  generated_at: string
  instance_id: string
  limit: number
  sandboxes: FleetSandboxClaim[]
}

// FleetSandboxSample is one tick of a single sandbox's in-run series
// (GET /api/fleet/claims/{id}/series). CPU arrives CUMULATIVE: the consumer
// differences consecutive samples into a rate, so a dropped tick self-heals
// into a wider-but-correct interval instead of a gap that reads as idle.
export interface FleetSandboxSample {
  at: string
  mem_current_mb?: number
  cpu_usec_cum?: number
}

export interface FleetSandboxSeries {
  generated_at: string
  claim_id: string
  samples: FleetSandboxSample[]
}

// UsageOrgOps — the org-scoped operations subset (TFAC-589): an org admin's own
// queue waits + run durations, mirroring GET /api/usage/org/ops. SaaS-safe (no
// cross-tenant machine truth).
export interface UsageOrgOpsFailureKind {
  kind: string
  count: number
}

export interface UsageOrgOps {
  window_since: string
  window_until: string
  queue_depth: number
  oldest_wait_seconds: number
  wait_p50_ms?: number
  wait_p95_ms?: number
  runs_total: number
  runs_active: number
  runs_completed: number
  runs_failed: number
  duration_p50_ms?: number
  duration_p95_ms?: number
  failure_kinds: UsageOrgOpsFailureKind[]
}
