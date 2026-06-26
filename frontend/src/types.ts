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
  // Outcome is the parsed terminal-envelope outcome
  // (continue|finish|abort), persisted to runs.outcome. Empty/absent for an
  // infra-error run or a step that ended without a recognized conclusion.
  // The blueprint run timeline reads this in place of the old verdict object.
  Outcome?: string
  // OutcomeReason is the "why I stopped" populated only on an abort outcome.
  OutcomeReason?: string
  SessionID?: string
  WorktreePath?: string
  // pending_kind is set by the server's runResponse projection when
  // status == 'pending_approval'. The discriminator tells the Board
  // which approval card variant to render: a queued review opens
  // ReviewOverlay (with inline-comment editing); a draft PR opens
  // PendingPROverlay (title/body editor + Open-PR button). Empty /
  // undefined for non-pending runs.
  pending_kind?: 'review' | 'pr'
  // pending_artifact_id is the id of the gating artifact — the review artifact
  // (pending_kind === 'review') or the draft-PR artifact (pending_kind === 'pr').
  // Both overlays are addressed by it (the artifact id), not the run id.
  pending_artifact_id?: string
  // artifact_count is the number of artifacts this run produced (TFAC-465's
  // runResponse projection — branch / PR / review / issue / comment, the
  // primary gating one included). The Board card shows it as a footer
  // affordance without a per-card fetch; 0 / undefined hides the affordance.
  artifact_count?: number
  blueprint_run_id?: string
  blueprint_step_index?: number | null
}

// ArtifactKind is the closed set of artifact discriminators the backend emits
// (internal/domain/artifact.go). Kept a strict union — not widened with
// `| string`, which would collapse the whole type to `string` and erase the
// narrowing. A server value outside this set is handled defensively at the
// render layer (a fallback icon/label), it just isn't part of the documented
// contract.
export type ArtifactKind = 'branch' | 'pull_request' | 'review' | 'issue' | 'comment'

// Artifact mirrors the GET /api/agent/runs/{id}/artifacts wire shape
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
  subtype: string // "" | "context_change" | ...
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
  brief: string
  // Optional: a step rebuilt from a blueprint run's frozen step_plan has no
  // live blueprint_steps row, so the run projection omits created_at (the
  // /steps editor reads still carry it).
  created_at?: string
}

export interface BlueprintRunStepView {
  step: BlueprintStep
  run?: AgentRun
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

/** GET /api/orgs/{org}/identity/github — the onboarding gate's status read.
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

/** GET /api/orgs/{org}/identity/jira — the Jira sibling of GitHubIdentityStatus.
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
  /** Whether one-click Connect is offerable. False until Cloud OAuth lands
   *  (DC = paste-a-PAT), so the surfaces offer only the token path for now. */
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

/** The body the accept endpoint returns on the 409 wrong-identity case: the
 *  recipient is signed in as someone other than the invited address. Carries
 *  the actionable message plus the address the invite was sent to so the page
 *  can offer "log out and sign in as {invited_email}". */
export interface AcceptInviteMismatch {
  error: string
  invited_email: string
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
  // Artifact reconciliation (TFAC-464): an artifact a run produced changed
  // state on GitHub (PR merged/closed, branch deleted, review submitted). The
  // run's own status is unchanged — consumers refetch the run to pick up its
  // artifact-derived surface (pending kind / approval card). Distinct from
  // agent_run_update precisely so it never feeds the Board's optimistic
  // run.Status write.
  | { type: 'artifact_updated'; run_id: string; data: { artifact_id: string; state: string } }
  | { type: 'agent_message'; run_id: string; data: AgentMessage }
  | {
      // P3 steering: a run surfaced a tool-permission prompt (canUseTool),
      // answered via POST /api/agent/runs/{runID}/permissions/{requestID}.
      // timeout_ms is the prompt's server-side deadline (relative); the dock
      // derives its dismiss TTL from it.
      type: 'permission_request'
      run_id: string
      data: {
        request_id: string
        tool_name: string
        input: Record<string, unknown>
        timeout_ms?: number
      }
    }
  | {
      // A pending permission prompt reached a terminal resolution (answered by
      // someone, or timed out) — broadcast so every surface showing it (board +
      // run-detail, or two board tabs) drops it promptly instead of waiting for
      // its own client TTL. The client TTL stays as a backstop.
      type: 'permission_resolved'
      run_id: string
      data: { request_id: string }
    }
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
