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

// FAILED_STATUSES — the terminals, styled rose rather than neutral. Every
// terminal is a failure: a conversation never concludes, so the only state it
// never leaves is the runtime under it dying. A future terminal lands here by
// default, which is the safe direction.
export const FAILED_STATUSES = TERMINAL_CONVERSATION_STATUSES

// isConcluded — the conversation is parked with its step's verdict: `open`,
// with the conclusion stamped. Mirrors domain.Conversation.Concluded. A
// concluded conversation is still resumable; whether the work it belongs to is
// done is its blueprint run's status, which completionKind reads.
export function isConcluded(conversation: Conversation): boolean {
  return conversation.Status === 'open' && conversation.CompletedAt != null
}

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

// PHASE_PROSE — what a setting-up conversation is doing, in the words the
// state actually means. One table for every surface that narrates a live
// conversation (an Overview row, a board card), so the same run never reads
// as two different procedures on two pages.
export const PHASE_PROSE: Record<ClaimPhase, string> = {
  fetching: 'Fetching the workspace',
  cloning: 'Cloning the repository',
  agent_starting: 'Starting the agent',
  awaiting_credentials: 'Waiting on credentials',
}

// activeProse — the one prose line an ACTIVE conversation shows in place of
// the title it does not have. A setup phase names the step it is on, and it
// wins over the action line: on a resume the newest transcript row still
// carries the previous turn's last tool call, and a conversation cloning its
// workspace is not editing anything. A running conversation leads with the
// server's derived current_action, and the honest fallback when it had
// nothing to say yet — the agent is live and thinking, its first tool call
// still to come — is the bare state word, never a fabricated action. Not
// active (queued, parked, ended) is undefined: each surface has its own
// words for those.
export function activeProse(conversation: Conversation): string | undefined {
  const s = conversation.Status
  if (isClaimPhase(s)) return PHASE_PROSE[s]
  if (s === 'running') return conversation.current_action || 'Working'
  return undefined
}

// A run that returning its task to the queue would STOP: minted and waiting
// for a slot, setting up, or executing a turn. `open` is excluded — a parked
// conversation is between turns, and ending it stops nothing — and so are the
// terminals. The board confirms a requeue on this, not on whether a run
// exists: with nothing in flight the move is reversible by re-claiming.
export function isLiveRun(conversation: Conversation): boolean {
  return conversation.Status === 'queued' || isActiveStatus(conversation.Status)
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

// CompletionKind splits a concluded conversation into what its verdict meant
// for the work. Three different endings land there, and collapsing them to one
// word is how a mid-chain step came to read as the whole task finishing:
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
//
// The verdict is the fallback, though, not the first word. A conversation never
// concludes, so the blueprint run's status is what says whether the work is
// done, and where the server could read it, it decides every step except one
// that handed off: a finished run is done even when a later follow-up on its
// last step reported something else, and an aborted, failed or cancelled run
// stopped whatever the step said before it. Only a run still `running` (its
// reactor has not acted on the verdict yet) or one the server could not read
// falls through to the verdict switch.
export type CompletionKind = 'done' | 'handoff' | 'stopped'

export function completionKind(conversation: Conversation): CompletionKind | null {
  if (!isConcluded(conversation)) return null
  const pos = chainPosition(conversation)
  const isFinal = pos ? pos.isFinal : true
  if (conversation.Outcome === 'continue' && !isFinal) return 'handoff'
  switch (conversation.blueprint_run_status) {
    case 'completed':
      return 'done'
    case 'aborted':
    case 'failed':
    case 'cancelled':
      return 'stopped'
  }
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

// completionGloss — the plain-language line for a concluded conversation, in
// one place so the dock and the telemetry rail can't tell the viewer two
// different stories about the same row. Empty for a conversation that hasn't
// concluded.
export function completionGloss(conversation: Conversation): string {
  const kind = completionKind(conversation)
  if (!kind) return ''
  const pos = chainPosition(conversation)
  if (kind === 'stopped') {
    // The ways to stop are different news: the workflow was cancelled or
    // failed around the step, the agent decided to stop, or it ended on
    // nothing the workflow could act on (no outcome recorded, or one this
    // build doesn't know). The rail prints the raw token beside this, so the
    // last line doesn't repeat it.
    if (conversation.blueprint_run_status === 'cancelled') {
      return 'workflow cancelled — the task stays open for a human'
    }
    if (conversation.blueprint_run_status === 'failed') {
      return 'the workflow failed — the task stays open for a human'
    }
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
  // A finished run stays finished: a follow-up on its last step that ended on
  // an abort is recorded on the step and reopens nothing.
  if (conversation.Outcome === 'abort')
    return 'work complete — a later follow-up’s abort reopened nothing'
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
  stalled: 'stalled',
  upstream_unavailable: 'Paused: provider unavailable',
  invalid_envelope: 'its completion envelope never validated — send a message to try again',
}

// parkReasonLabel is the gloss for one park reason. An unrecognized code
// prints as stored rather than being hidden: a value this build has never
// heard of is still the only account of why the conversation stopped.
export function parkReasonLabel(reason: string): string {
  return PARK_REASON_LABELS[reason] ?? reason
}

// isResumableConversation mirrors the backend resumableState gate — the STATUS half of
// resumability, and the cheap first cut only. A conversation with no live turn can be
// woken by a follow-up when it parked `open`, concluded or not: an abort is
// work a human picks back up, a finish is work a human follows up on, and both
// have a workspace to land in because every verdict snapshots. `failed` is the
// exclusion — the infrastructure under it died, so there is no tree to
// rehydrate.
//
// Status is one of three inputs, and the only one the client can see: whether
// the workspace survived and whether anything would drive it are server-side
// facts. So this is never the whole answer — use canResumeConversation, which folds in
// the server's own verdict.
export function isResumableConversation(conversation: Conversation): boolean {
  return conversation.Status === 'open'
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
    // The boundary rungs. Each says the same thing — this conversation is no
    // longer the one its task is about — and then the part that differs: who
    // or what carries the work now, since that is what the reader does next.
    case 'ended_requeued':
      return 'This task was returned to the queue; the next claimant starts a new conversation.'
    case 'ended_delegated':
      return 'This task was handed to a new delegation, which carries the work now.'
    case 'ended_taken_over':
      return 'Someone took this task over, so this run is finished. Follow up with them.'
    case 'ended_step_advanced':
      return 'This step is done and the workflow has moved on — follow up on its latest step.'
    case 'ended_team_archived':
      return 'The team that owned this work was archived, so nothing will pick it back up.'
    case 'ended_failed':
      return 'This run failed, so it can’t be continued. Delegate the task again to start fresh.'
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
// (ClaimedAt − QueuedAt). QueuedAt is stamped when the conversation entered the
// queue this episode — a resume re-stamps it at the wake — so the readout is
// the current wait, never the whole life since mint. A row that predates the
// queue columns has no QueuedAt: while it is live in the queue its wait is
// measured from StartedAt (the only stamp it has, and for such a row also its
// enqueue), and once settled its dwell is unknowable, so null.
export function queueDwellMs(conversation: Conversation, now: number = Date.now()): number | null {
  const queuedAt =
    conversation.QueuedAt ?? (conversation.Status === 'queued' ? conversation.StartedAt : null)
  if (!queuedAt) return null
  if (conversation.Status === 'queued') return Math.max(0, now - new Date(queuedAt).getTime())
  if (!conversation.ClaimedAt) return null
  return Math.max(0, new Date(conversation.ClaimedAt).getTime() - new Date(queuedAt).getTime())
}

// retryingAt is when a deferred conversation is tried again: a queued
// conversation whose model provider was unavailable, handed back to wait until
// next_attempt_at. It reads `queued` and has no place in line, because it waits
// for that time rather than for the runs ahead of it. Null for every other
// conversation, and once the time has passed — from then on it is an ordinary
// queued run waiting for a slot.
export function retryingAt(
  conversation: Pick<Conversation, 'Status' | 'next_attempt_at'>,
  now: number = Date.now(),
): Date | null {
  if (conversation.Status !== 'queued' || !conversation.next_attempt_at) return null
  const at = new Date(conversation.next_attempt_at)
  if (Number.isNaN(at.getTime()) || at.getTime() <= now) return null
  return at
}

// retryingAtLabel renders retryingAt's time for a person, in their local time
// and the 24-hour clock the run views stamp times with.
export function retryingAtLabel(at: Date): string {
  return `Retrying at ${at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })}`
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

// claimIdleReadout — how long a claim's engagement has gone without anything
// its stall watchdog counts as activity, then the operation it had in flight:
// "3m 0s · tool:bash", or "3m 0s" with nothing in flight. Both inputs are what
// the holder's last lease renewal stamped. Null without a stamp: there is no
// claim, or it has not renewed yet. A browser clock behind the server's reads
// as zero rather than a negative duration. The caller decides whether the
// claim is live: a released claim keeps its last reading, which says nothing
// about now.
export function claimIdleReadout(
  lastActivityAt: string | undefined,
  currentOp: string | undefined,
  now: number,
): string | null {
  if (!lastActivityAt) return null
  const idle = formatDurationMs(Math.max(0, now - new Date(lastActivityAt).getTime()))
  return currentOp ? `${idle} · ${currentOp}` : idle
}

// claimCheckpointReadout — how long a claim's engagement has gone since its
// workspace was last covered by a stored checkpoint: the work a hard kill of
// its executor would lose. Null without a stamp — an engagement that does not
// checkpoint, or one that has not renewed yet. Skew clamps to zero, and the
// caller decides liveness, both as claimIdleReadout does.
export function claimCheckpointReadout(
  lastCheckpointAt: string | undefined,
  now: number,
): string | null {
  if (!lastCheckpointAt) return null
  return formatDurationMs(Math.max(0, now - new Date(lastCheckpointAt).getTime()))
}
