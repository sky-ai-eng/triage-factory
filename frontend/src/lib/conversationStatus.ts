import { CLAIM_PHASES, TERMINAL_CONVERSATION_STATUSES } from '../types'
import type {
  ClaimPhase,
  Conversation,
  ConversationStatusValue,
  TerminalConversationStatus,
} from '../types'

// Every set here is derived from the vocabulary in types.ts, which is checked
// against internal/domain/conversation_status.go by a Go test. Adding a claim phase in
// one place therefore lands in every predicate below at once.

// ACTIVE_STATUSES — the conversation is claimed and occupying an executor slot: setting
// up, or executing a turn. Mirrors domain.IsActiveConversationStatus, which is likewise
// `running` plus every claim phase. `queued` is excluded (waiting, not
// working) and so is `open` (parked between turns).
export const ACTIVE_STATUSES = ['running', ...CLAIM_PHASES] as const

// FAILED_STATUSES — the terminals that are not a success, styled rose rather
// than neutral. Derived by excluding the one success rather than re-listing
// the other three, so a renamed terminal can't quietly fall out of it; a
// future terminal lands here by default, which is the safe direction.
export const FAILED_STATUSES = TERMINAL_CONVERSATION_STATUSES.filter((s) => s !== 'completed')

export function isTerminalStatus(
  status: ConversationStatusValue,
): status is TerminalConversationStatus {
  return (TERMINAL_CONVERSATION_STATUSES as readonly string[]).includes(status)
}

// A conversation that reached a terminal is no longer executing a turn, so any
// tool-permission prompt parked on it is stale and must be dropped — its
// Allow/Deny would 404 or resolve a now-meaningless prompt. Its own name
// because the question is about permission staleness, not lifecycle; the two
// answers coincide today. Shared by the board and useConversationDetail so the two
// surfaces drop in lockstep.
export function isPermissionTerminalStatus(status: ConversationStatusValue): boolean {
  return isTerminalStatus(status)
}

export function isClaimPhase(status: ConversationStatusValue): status is ClaimPhase {
  return (CLAIM_PHASES as readonly string[]).includes(status)
}

export function isActiveConversation(conversation: Conversation): boolean {
  return isActiveStatus(conversation.Status)
}

export function isActiveStatus(status: ConversationStatusValue): boolean {
  return (ACTIVE_STATUSES as readonly string[]).includes(status)
}

export function isFailedStatus(status: ConversationStatusValue): boolean {
  return (FAILED_STATUSES as readonly string[]).includes(status)
}

// ChainPosition — where a conversation sits in its blueprint's frozen step
// plan. Every delegated conversation belongs to a blueprint (the schema requires
// it), so a plan of one step IS the plain single-prompt case: that reads as no chain at
// all, and returns null here.
//
// null also covers "position unknown": blueprint_step_count is 0 when the
// server could not resolve the plan (a manual blueprint run belongs to its
// creator under RLS, so a teammate reads 0), and a conversation predating the field
// carries neither. Callers fall back to the unqualified reading rather than
// guessing a position.
export interface ChainPosition {
  /** 1-based, for display — "step 2 of 4". */
  step: number
  total: number
  /** The last step of the plan: nothing runs after this one. */
  isFinal: boolean
}

export function chainPosition(conversation: Conversation): ChainPosition | null {
  const total = conversation.blueprint_step_count ?? 0
  const index = conversation.blueprint_step_index
  if (total <= 1 || index == null || index < 0) return null
  return { step: index + 1, total, isFinal: index >= total - 1 }
}

// CompletionKind splits the one stored success terminal into what it actually
// meant. `completed` is where three different endings land, and collapsing them
// to one word is how a mid-chain step came to read as the whole task finishing:
//
//   - 'handoff' — a step of a chain that isn't the last one, whose part is done.
//     Another step picks the work up; the task is NOT over.
//   - 'stopped' — the work stopped without finishing and the task stays open for
//     a human: the agent aborted, or a non-final step ended with no usable
//     hand-off.
//   - 'done'    — the work is over. The chain's final step, a single-prompt
//     conversation,
//     or a step that deliberately ended the whole workflow early ('finish').
//
// Position, not outcome, is what separates the first from the third, because
// `continue` is what an ORDINARY completion records — the native loop stamps it
// on every step including the last (internal/agentloop), while under the SDK a
// terminal step reports `finish`.
//
// The arms below mirror decideBlueprintStep (internal/delegate/blueprint.go)
// one for one, because that function is what actually decides whether anything
// runs after this conversation — so it is written as the same switch over the
// same two inputs rather than as a position test with an outcome test inside
// it. Only two of its arms consult the position, and the difference is
// load-bearing:
//
//   - `continue` / `''` branch on it. Handing off needs a step to hand off TO,
//     so on the final step both resolve to a plain finish, and only before it
//     does `continue` advance and a missing outcome abort ("no-outcome").
//   - `abort`, and anything the vocabulary does not recognize, abort the
//     blueprint wherever they land. An unrecognized value is a name this build
//     predates or a bug, and the orchestrator refuses to close a task on one
//     at any position — reading it as 'done' on the last step would put a green
//     verdict on a blueprint that actually aborted.
//
// A position we cannot read at all (chainPosition null — an unresolved plan
// length) is the one genuine unknown. It reads as final, which is the
// conservative direction for the two arms that care and irrelevant to the two
// that don't.
export type CompletionKind = 'done' | 'handoff' | 'stopped'

export function completionKind(conversation: Conversation): CompletionKind | null {
  if (conversation.Status !== 'completed') return null
  const pos = chainPosition(conversation)
  const isFinal = pos ? pos.isFinal : true
  switch (conversation.Outcome) {
    case 'continue':
      return isFinal ? 'done' : 'handoff'
    case 'finish':
      return 'done'
    case 'abort':
      return 'stopped'
    case '':
    case undefined:
      return isFinal ? 'done' : 'stopped'
    default:
      return 'stopped'
  }
}

// completionGloss — the plain-language line for a settled conversation, in one
// place so the dock and the telemetry rail can't tell the viewer two
// different stories about the same row. Empty for a conversation that hasn't completed.
export function completionGloss(conversation: Conversation): string {
  const kind = completionKind(conversation)
  if (!kind) return ''
  const pos = chainPosition(conversation)
  if (kind === 'stopped') {
    // Two ways to stop, and they are different news: the agent decided to,
    // or it ended on nothing the workflow could act on (no outcome recorded,
    // or one this build doesn't know). The rail prints the raw token beside
    // this, so the second line doesn't repeat it.
    return conversation.Outcome === 'abort'
      ? 'stopped without finishing — the task stays open for a human'
      : 'ended without a usable outcome — the workflow stopped here for a human'
  }
  if (kind === 'handoff') {
    return `step ${pos!.step} of ${pos!.total} done — handed off to the next step`
  }
  // Only an explicit `finish` reaches this from mid-chain, so the sentence is
  // safe to say: the agent chose to end the workflow. Every other non-final
  // ending is 'stopped' above and must never be described as a deliberate one.
  if (pos && !pos.isFinal) return 'ended the workflow early — the later steps were skipped'
  if (pos) return `work complete — the last of ${pos.total} steps`
  return 'work complete'
}

// PARK_REASON_LABELS glosses conversations.park_reason for display: WHY a
// conversation stopped without concluding. The stored values are machine
// vocabulary and printing one raw tells a viewer their conversation was `user_cancelled`
// — which reads as "cancelled", when a park cancels nothing: the conversation
// keeps its workspace and can be picked back up. Every phrase below is written
// to say that.
//
// This is the frontend mirror of domain.AllParkReasons() (internal/domain/
// conversation_status.go), hand-maintained under the same rule as the status arrays in
// types.ts and pinned in both directions by
// TestFrontendMirrorsParkReasonVocabulary — a reason the backend can write with
// no entry here is printed to a person as a raw identifier, and an entry the
// backend never writes is a gloss nobody will ever see.
export const PARK_REASON_LABELS: Record<string, string> = {
  idle: 'paused — nothing further came',
  user_cancelled: 'stopped by user',
  system_cancelled: 'stopped by system',
  blueprint_cancelled: 'workflow cancelled',
  blueprint_terminal: 'workflow ended first',
  launch_failed: 'the runtime could not start',
  model_not_enabled: 'its model is no longer one this team can pick',
  drained: 'the executor was drained',
}

// parkReasonLabel is the gloss for one park reason. An unrecognized code
// prints as stored rather than being hidden: a value this build has never
// heard of is still the only account of why the conversation stopped.
export function parkReasonLabel(reason: string): string {
  return PARK_REASON_LABELS[reason] ?? reason
}

// isResumableConversation mirrors the backend resumableState gate — the STATUS half of
// resumability, and the cheap first cut only. A conversation with no live turn can be
// woken by a follow-up when it parked `open` or when it concluded: an abort is
// work a human picks back up, a finish is work a human follows up on, and both
// have a workspace to land in because every completed terminal snapshots.
// `failed` is the exclusion — the infrastructure under it died, so there is no
// tree to rehydrate.
//
// Outcome no longer discriminates, which is why this reads as status alone.
//
// Status is one of three inputs, and the only one the client can see: whether
// the workspace survived and whether anything would drive it are server-side
// facts. So this is never the whole answer — use canResumeConversation, which folds in
// the server's own verdict.
export function isResumableConversation(conversation: Conversation): boolean {
  return conversation.Status === 'open' || conversation.Status === 'completed'
}

// canResumeConversation is the composer's gate for a conversation with no live
// turn: the cheap status cut above, then the server's answer, which is
// authoritative.
//
// `!== false` rather than `=== true` is the compatibility rule the field is
// specified with: the conversation read omits `resumable` for rows it doesn't compute it
// for (active, failed) and any response that predates the field carries none.
// Absent means "the server didn't answer" — fall back to the status reading —
// while an explicit false is a real refusal with a reason attached.
export function canResumeConversation(conversation: Conversation): boolean {
  return isResumableConversation(conversation) && conversation.resumable !== false
}

// resumeBlockedCopy — what to tell someone whose conversation looks resumable
// but isn't, in the words the 410/409 on send would have used. The reason comes
// from the server (internal/delegate's ResumeBlocked* rungs); an unrecognized
// one falls through to the generic sentence, since a build that predates a new
// rung still has to say something true.
export function resumeBlockedCopy(conversation: Conversation): string {
  switch (conversation.resume_blocked_reason) {
    case 'workspace_expired':
      return 'Workspace expired — this conversation can’t be resumed.'
    case 'blueprint_concluded':
      return 'This blueprint has moved past this step — only the step it’s on takes follow-ups.'
    case 'blueprint_cancelled':
      return 'This run’s workflow was cancelled, so it can’t take follow-ups. The note in the transcript says what ended it.'
    case 'step_handed_off':
      return 'This step just handed off — follow up on the blueprint’s latest step.'
    case 'session_missing':
    case 'worktree_missing':
    case 'model_missing':
      return 'This run didn’t record the state a resume needs, so it can’t be continued.'
    case 'model_not_enabled':
      return 'This run’s model is no longer one this team can pick — choose one from the team’s enabled models in Settings, then continue.'
    default:
      return 'This conversation can’t take a follow-up.'
  }
}

// workStartedAt — when the conversation actually began executing: the dispatcher's
// claim stamp, falling back to the mint stamp for legacy rows that predate the
// queue columns. Live elapsed readouts tick from here so queue dwell never
// inflates working time.
export function workStartedAt(conversation: Conversation): string {
  return conversation.ClaimedAt ?? conversation.StartedAt
}

// Settled queue dwell below this stays off the display surfaces (card footer,
// telemetry rail): a couple of seconds is normal dispatch latency (the claim
// scan tick), not a wait worth a readout. One constant so the two surfaces
// can't drift; a live QUEUED conversation always shows its wait regardless.
export const QUEUE_DWELL_VISIBLE_MS = 5000

// queueDwellMs — how long the conversation waited in the queue: live (now − QueuedAt)
// while it is still queued, else the latest episode's settled dwell
// (ClaimedAt − QueuedAt). null when the row predates the queue columns and the
// dwell is unknowable.
export function queueDwellMs(conversation: Conversation, now: number = Date.now()): number | null {
  const queuedAt =
    conversation.QueuedAt ?? (conversation.Status === 'queued' ? conversation.StartedAt : null)
  if (!queuedAt) return null
  if (conversation.Status === 'queued') return Math.max(0, now - new Date(queuedAt).getTime())
  if (!conversation.ClaimedAt) return null
  return Math.max(0, new Date(conversation.ClaimedAt).getTime() - new Date(queuedAt).getTime())
}

export function formatDurationMs(ms: number): string {
  const seconds = Math.floor(ms / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  if (minutes < 60) return `${minutes}m ${secs}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

export function formatElapsed(dateStr: string, now: number = Date.now()): string {
  const diff = now - new Date(dateStr).getTime()
  const seconds = Math.floor(diff / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  if (minutes < 60) return `${minutes}m ${secs}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}
