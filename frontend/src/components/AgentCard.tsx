import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router'
import * as Popover from '@radix-ui/react-popover'
import type { Conversation, Task } from '../types'
import { EMPTY_FEED, type FeedLine, type RunCardFeed } from '../lib/runFeed'
import { useOrgHref } from '../hooks/useOrgHref'
import ArtifactList from './ArtifactList'
import RequestedReviewerBadge from './RequestedReviewerBadge'
import { toast } from './Toast/toastStore'
import { apiFetch, httpErrorMessage } from '../lib/apiClient'
import {
  chainPosition,
  completionKind,
  formatDurationMs,
  formatElapsed,
  isActiveRun,
  isActiveStatus,
  isFailedStatus,
  isTerminalStatus,
  QUEUE_DWELL_VISIBLE_MS,
  queueDwellMs,
  workStartedAt,
} from '../lib/runStatus'
import { AttentionRow, CardPlane, EventTag, HudHeader, SourceTag } from './board/cardChrome'
import {
  compactNum,
  runGlow,
  TONE_TEXT,
  type Glow,
  type StepState,
  type Tone,
} from './board/cardStyle'
import { PermissionPrompt } from './permissions/PermissionPrompt'
import type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'
import {
  approvalAction,
  approvalCounts,
  approvalKicker,
  artifactSetKey,
  hasUnresolvedArtifacts,
} from '../lib/approval'

interface Props {
  task: Task
  run: Conversation
  chainSteps?: Conversation[]
  // Bounded live-feed projection for this run (running stats + last few ticker
  // lines) — see lib/runFeed. The board maintains it incrementally instead of
  // holding every run's full message array in state.
  feed?: RunCardFeed
  // Unanswered tool-permission prompts for this run, head-first. When non-empty
  // the card renders an inline Allow/Deny control and takes the attention tone.
  pendingPermissions?: PendingPermission[]
  onResolvePermission?: (toolCallID: string, decision: PermissionDecisionInput) => Promise<void>
  onRequeue?: () => void
  // Open a PR/review artifact's approval overlay by id — from a row in the
  // artifacts popover, or directly from the attention row when exactly one
  // item is unresolved.
  onOpenArtifact?: (kind: 'review' | 'pr', artifactId: string) => void
  // Re-pull the board's task/run projections after an in-place dismiss in the
  // artifacts popover (the popover's list refetches itself; the card's counts
  // and column live on the run row).
  onArtifactResolved?: () => void
  // Caller-supplied assignee picker, rendered in the header cluster.
  assigneeSlot?: React.ReactNode
}

// AgentCard — the lit plane for a task that has a run on it. The spine color is
// the run's tone (delegate-violet while turning, amber waiting, red failed,
// green done); a live run breathes via CardPlane's glow, so the board reads
// "work is alive here" from the card itself, not the column. Status words live
// only where they carry weight: the attention rows (your move) and the result
// summary (terminal outcome) — the header stays clean.
export default function AgentCard({
  task,
  run,
  chainSteps,
  feed,
  pendingPermissions,
  onResolvePermission,
  onRequeue,
  onOpenArtifact,
  onArtifactResolved,
  assigneeSlot,
}: Props) {
  const orgHref = useOrgHref()
  const [now, setNow] = useState(() => Date.now())
  // The footer's artifacts popover, lifted so the attention row can raise it
  // too: for a plural/mixed unresolved set the popover IS the approval surface
  // (unresolved rows sort to the top of the list).
  const [artifactsOpen, setArtifactsOpen] = useState(false)

  const isActive = isActiveRun(run)
  // Waiting for a dispatcher slot (the process-wide concurrency cap), not
  // executing. Without an explicit state the card reads as broken — a frozen
  // elapsed readout and an empty feed — when the run is just in line.
  const isQueued = run.Status === 'queued'

  // Tick once per second while active (elapsed) or queued (wait time) so the
  // readout updates live.
  useEffect(() => {
    if (!isActive && !isQueued) return
    const interval = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(interval)
  }, [isActive, isQueued])

  // Queue dwell is its own timer: a queued run's header ticks the live wait,
  // a started run's footer shows the settled dwell. Working elapsed ticks
  // from the claim stamp so queue time never inflates it.
  const elapsed = isQueued
    ? formatElapsed(run.QueuedAt ?? run.StartedAt, now)
    : !isActive && run.DurationMs != null
      ? formatDurationMs(run.DurationMs)
      : formatElapsed(workStartedAt(run), now)
  const dwellMs = queueDwellMs(run, now)
  // Approval is derived from the unresolved-artifact set (draft PRs + ready
  // reviews), never a run status — a card needs the user
  // whenever has_unresolved_artifacts is true, live or terminal.
  const needsApproval = hasUnresolvedArtifacts(run)
  const approval = approvalCounts(run)
  const isFailed = isFailedStatus(run.Status)
  // Parked: the turn ended without concluding — stopped from this card's own ✕,
  // or ended mid-thought — so the run is neither working nor settled. Its
  // workspace stays warm and it is resumable from the run view, which is why it
  // is NOT a terminal and gets its own block rather than a stale ticker.
  const isParked = run.Status === 'open'
  // A terminal run is settled — show its outcome, not a live feed.
  const isTerminal = isTerminalStatus(run.Status)

  // A parked tool-permission prompt OR an unresolved artifact set is the card's
  // "needs you" moment — it takes the steady amber attention glow so it can't be
  // missed in the column.
  const pending = pendingPermissions ?? []
  const hasPending = pending.length > 0
  const glow: Glow | null =
    hasPending || needsApproval ? { tone: 'attention', breathing: false } : runGlow(run)
  const stats = feed ?? EMPTY_FEED

  const hasChain = !!chainSteps && chainSteps.length > 1
  const stepStates: StepState[] = useMemo(
    () =>
      hasChain ? chainStepStates(chainSteps!, run.ID, run.blueprint_step_index ?? undefined) : [],
    [hasChain, chainSteps, run.ID, run.blueprint_step_index],
  )
  return (
    <div className="relative">
      <CardPlane glow={glow}>
        {/* Header label plate — its divider doubles as the chain progress track. */}
        <HudHeader steps={hasChain ? stepStates : undefined}>
          <div className="flex items-center justify-between gap-2">
            <div className="flex min-w-0 items-center gap-2.5">
              <SourceTag task={task} />
              <EventTag eventType={task.event_type} />
              <RequestedReviewerBadge task={task} />
            </div>
            <div className="flex shrink-0 items-center gap-2">
              {assigneeSlot}
              <HeaderDivider />
              <span
                className="inline-flex items-center gap-1.5 font-mono text-[11px] leading-none tabular-nums text-text-tertiary/80"
                title={isQueued ? 'Time spent waiting in the run queue' : undefined}
              >
                {isActive && (
                  <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-delegate" />
                )}
                {isQueued && (
                  <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-snooze" />
                )}
                {elapsed}
              </span>
              <HeaderDivider />
              <Link
                to={orgHref(`/runs/${run.ID}`)}
                target="_blank"
                rel="noopener noreferrer"
                aria-label="Expand run details"
                title="Open full session view (new tab)"
                className="inline-flex items-center text-text-tertiary transition-colors hover:text-text-primary"
              >
                <svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden>
                  <path
                    d="M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5l-5.5 5.5M2.5 13.5l5.5-5.5"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
              </Link>
              {isActive && (
                <>
                  <HeaderDivider />
                  <button
                    type="button"
                    onClick={async () => {
                      try {
                        await apiFetch(`/api/agent/conversations/${run.ID}/stop`, {
                          method: 'POST',
                        })
                      } catch (err) {
                        toast.error(httpErrorMessage(err, 'Could not stop the run.'))
                      }
                    }}
                    className="inline-flex items-center text-dismiss/40 transition-colors hover:text-dismiss"
                    title="Cancel run"
                  >
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none">
                      <circle cx="8" cy="8" r="7" stroke="currentColor" strokeWidth="1.5" />
                      <path
                        d="M5.5 5.5l5 5M10.5 5.5l-5 5"
                        stroke="currentColor"
                        strokeWidth="1.5"
                        strokeLinecap="round"
                      />
                    </svg>
                  </button>
                </>
              )}
            </div>
          </div>
        </HudHeader>

        {/* Title + entity readout */}
        <div className="px-4 pb-2 pt-3">
          <h3 className="mb-1 line-clamp-2 text-[14px] font-semibold leading-snug text-text-primary">
            {task.title}
          </h3>
          <div className="flex min-w-0 items-center gap-2 text-[11px] text-text-tertiary/80">
            <span className="truncate font-mono">{task.source_id}</span>
          </div>
        </div>

        {/* Activity — a queued run states that it's waiting (a slot, not a
            bug); a parked run states that it stopped and can be picked back
            up; a terminal run shows its outcome; a live run shows a
            flush, borderless feed of one-liners (no window-into-the-agent; the
            expanded view is for that). A memory-limit kill has no
            ResultSummary (infra failures write run_messages, not a summary)
            but still gets the ResultBlock — its kind carries the copy. */}
        {isQueued ? (
          <QueuedBlock />
        ) : isParked ? (
          <ParkedBlock />
        ) : isTerminal && (run.ResultSummary || run.FailureKind === 'memory_limit') ? (
          <ResultBlock run={run} />
        ) : (
          <LiveFeed lines={stats.lines} isActive={isActive} />
        )}

        {/* Inline tool-permission prompt — the parked turn's Allow/Deny,
            answerable without opening the run. Head of queue + "N more". */}
        {hasPending && (
          <div className="mx-4 mb-2">
            <PermissionPrompt
              key={pending[0].tool_call_id}
              prompt={pending[0]}
              remaining={pending.length - 1}
              worktree={run.WorktreePath}
              onResolve={onResolvePermission}
              compact
            />
          </div>
        )}

        {/* Attention row — your move, in the canonical toned pattern. Count-aware
            over the unresolved set. One unresolved item goes straight to its
            editor — no list detour; pending_artifact_ids lists draft PRs first,
            then ready reviews, so unresolved_pr_count discriminates the head's
            kind. Anything else (plural, mixed, or a transiently absent id set)
            raises the artifacts popover, unresolved rows first. */}
        {needsApproval && onOpenArtifact && (
          <div className="mx-4 mb-2">
            <AttentionRow
              kicker={approvalKicker(approval)}
              action={approvalAction(approval)}
              onClick={() => {
                const ids = run.pending_artifact_ids ?? []
                if (approval.total === 1 && ids.length > 0) {
                  onOpenArtifact((run.unresolved_pr_count ?? 0) > 0 ? 'pr' : 'review', ids[0])
                } else {
                  setArtifactsOpen(true)
                }
              }}
            />
          </div>
        )}

        {/* Footer */}
        <div className="flex items-center justify-between pb-3.5 pl-4 pr-4">
          <div className="flex items-center gap-3 font-mono text-[11px] tabular-nums tracking-wide text-text-tertiary/80">
            {/* Who executed this run, shown only once that diverges from who
                holds the task. The header's assignee chip already names the bot
                while it still holds the claim; once the claim moves off it — a
                takeover, a reassign, or a requeue that clears it outright — the
                run's actor stays frozen, and this line becomes the only record
                of who actually did the work. */}
            {run.actor_agent_name && run.actor_agent_id !== task.claimed_by_agent_id && (
              <span title="The bot that executed this run; the task is no longer claimed by it">
                Ran as {run.actor_agent_name}
              </span>
            )}
            {!isQueued && dwellMs != null && dwellMs >= QUEUE_DWELL_VISIBLE_MS && (
              <span title="Time this run waited in the queue for a free slot before starting">
                queued {formatDurationMs(dwellMs)}
              </span>
            )}
            {stats.tokens > 0 && <span>{compactNum(stats.tokens)} tokens</span>}
            {run.TotalCostUSD != null && run.TotalCostUSD > 0 && (
              <span>${run.TotalCostUSD.toFixed(3)}</span>
            )}
            <ArtifactsAffordance
              run={run}
              onOpenArtifact={onOpenArtifact}
              onArtifactResolved={onArtifactResolved}
              open={artifactsOpen}
              onOpenChange={setArtifactsOpen}
            />
          </div>

          <div className="flex items-center gap-3">
            {(isFailed || isParked || needsApproval) && onRequeue && (
              <button
                onClick={onRequeue}
                className="text-[12px] font-medium text-text-tertiary transition-colors hover:text-text-secondary"
              >
                Return to queue
              </button>
            )}
            {task.source_url && (
              <a
                href={task.source_url}
                target="_blank"
                rel="noopener noreferrer"
                className="text-[12px] font-semibold text-accent transition-colors hover:text-accent/70"
              >
                Open
              </a>
            )}
          </div>
        </div>
      </CardPlane>
    </div>
  )
}

// HeaderDivider is the hairline tick between the header's right-cluster
// readouts (assignee | elapsed | expand | cancel).
function HeaderDivider() {
  return <span aria-hidden className="h-3 w-px shrink-0 bg-text-tertiary/20" />
}

// ArtifactsAffordance is the footer's "N artifacts →" button — the full set a
// run produced (the count comes free with the run). Hidden at 0. Open state is
// owned by the card, because the attention row raises the same popover for a
// plural/mixed unresolved set. The popover lazy-fetches the list (ArtifactList
// only mounts when it does), with the run's unresolved rows sorted first and
// dismissable in place; PR/review rows reuse the board's approval overlays via
// onOpenArtifact, the rest link out.
function ArtifactsAffordance({
  run,
  onOpenArtifact,
  onArtifactResolved,
  open,
  onOpenChange,
}: {
  run: Conversation
  onOpenArtifact?: (kind: 'review' | 'pr', artifactId: string) => void
  onArtifactResolved?: () => void
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const count = run.artifact_count ?? 0
  // `open` keeps a forced-open popover alive through a transiently absent
  // count (the projections travel together, but the guard keeps the attention
  // row's raise from ever landing on an unmounted trigger).
  if (count <= 0 && !open) return null
  return (
    <Popover.Root open={open} onOpenChange={onOpenChange}>
      <Popover.Trigger asChild>
        <button
          type="button"
          className="font-mono text-[11px] tabular-nums tracking-wide text-accent transition-colors hover:text-accent/70"
          aria-label={`Show ${count} artifact${count === 1 ? '' : 's'}`}
        >
          {count} artifact{count === 1 ? '' : 's'} →
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          side="top"
          align="start"
          sideOffset={6}
          className="z-[100] max-h-[360px] w-[320px] overflow-y-auto rounded-xl border border-border-glass bg-surface-raised/95 p-2.5 shadow-lg shadow-black/[0.08] backdrop-blur-2xl animate-in fade-in-0 zoom-in-95"
        >
          <div className="mb-1.5 px-1 font-mono text-[9px] font-semibold uppercase tracking-[0.18em] text-text-tertiary/70">
            Artifacts
          </div>
          <ArtifactList
            runId={run.ID}
            pendingArtifactIds={run.pending_artifact_ids}
            refreshKey={artifactSetKey(run)}
            onOpenApproval={(kind, id) => {
              onOpenChange(false)
              onOpenArtifact?.(kind, id)
            }}
            onResolved={onArtifactResolved}
          />
          <Popover.Arrow className="fill-surface-raised" />
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

// QueuedBlock fills the feed slot while the run waits for a dispatcher slot.
// Concurrent runs are capped process-wide, so a burst of delegations executes
// a few at a time — this says so, instead of leaving a dead-looking card.
function QueuedBlock() {
  return (
    <div className="flex h-[3.5rem] items-end px-4 pb-1 font-mono text-[10.5px] text-text-tertiary/70">
      <span
        className="inline-flex items-center gap-2"
        title="Concurrent runs are capped (TF_MAX_CONCURRENT_RUNS, default 8). This run starts automatically when a slot frees up."
      >
        <span className="inline-block h-1 w-1 animate-pulse rounded-full bg-snooze" />
        queued — starts when a run slot frees up
      </span>
    </div>
  )
}

// ParkedBlock is the `open` run's honest state: the turn ended without
// concluding, nothing is executing, and the work is still there to pick up. It
// takes the feed's slot rather than sitting alongside it because a parked run's
// ticker is stale by definition — the last few lines read as live work when
// none is happening, which is exactly how the retired `cancelled` arm's absence
// would have left this state. The transcript is one click away in the run view,
// which is also where the composer lives; the footer's Return to queue is the
// other exit.
function ParkedBlock() {
  return (
    <div className="flex h-[3.5rem] items-end px-4 pb-1 font-mono text-[10.5px] text-text-tertiary/70">
      <span
        className="inline-flex items-center gap-2"
        title="The run stopped without concluding and its workspace is still warm. Open the run view to send it a follow-up, or return the task to the queue."
      >
        <span className="inline-block h-1 w-1 rounded-full bg-snooze" />
        idle — stopped without concluding, resumable
      </span>
    </div>
  )
}

// The feed fades in at its top edge so older lines dissolve as new ones arrive.
const FEED_MASK = 'linear-gradient(to bottom, transparent 0, #000 22px, #000 100%)'

// LiveFeed is the flush, borderless activity ticker: a few of the most recent
// one-liners in monospace, auto-advancing to the newest, top-masked so older
// lines dissolve upward. No box, no scrollbar — it's part of the card. The
// lines arrive pre-flattened (lib/runFeed) so the card does no per-render
// transcript work.
function LiveFeed({ lines, isActive }: { lines: FeedLine[]; isActive: boolean }) {
  if (lines.length === 0) {
    return (
      <div className="flex h-[3.5rem] items-end px-4 pb-1 font-mono text-[10.5px] text-text-tertiary/70">
        {isActive ? (
          <span className="inline-flex items-center gap-2">
            <span className="inline-block h-1 w-1 animate-pulse rounded-full bg-delegate" />
            awaiting agent…
          </span>
        ) : null}
      </div>
    )
  }

  // Bottom-anchored: the newest line always lands at the same spot at the
  // bottom; earlier ones stack above it and slide up (and out under the top
  // mask) as the feed grows. We keep only the last few — the mask hides the
  // rest, and the expanded run view holds the full history.
  const recent = lines.slice(-5)
  return (
    <div
      className="flex h-[3.5rem] flex-col justify-end overflow-hidden px-4"
      style={{ maskImage: FEED_MASK, WebkitMaskImage: FEED_MASK }}
    >
      {recent.map((l, i) => {
        const latest = i === recent.length - 1
        return (
          <div
            key={l.id}
            className="flex items-baseline gap-2 py-[1.5px] font-mono text-[10.5px] leading-relaxed"
          >
            <span className="shrink-0 tabular-nums text-text-tertiary/45">{l.time}</span>
            <span
              className={`truncate ${latest ? 'text-text-secondary' : 'text-text-tertiary/70'}`}
            >
              {latest && isActive && <span className="text-delegate">› </span>}
              {l.text}
            </span>
          </div>
        )
      })}
    </div>
  )
}

// ResultBlock is the settled-run outcome: a toned uppercase verdict + the
// agent's summary, flush in the card (no box). A classified memory-limit
// kill swaps the generic Failed verdict for a specific one and, when the run
// has no summary (infra failures don't write one), explanatory copy naming
// the knob to turn.
//
// The success terminal splits three ways (lib/runStatus completionKind), and
// the card says which: a mid-chain step that handed off is not the task being
// over, and an abort is not a success at all. Same derivation the run station's
// state light uses, so the board card and the run page agree.
function ResultBlock({ run }: { run: Conversation }) {
  const status = run.Status
  const summary = run.ResultSummary
  const memoryKilled = status === 'failed' && run.FailureKind === 'memory_limit'
  // Tone and heading come off the shared failed-terminal predicate rather than
  // a list of names, so a future terminal styles as a failure by default —
  // the safe direction, and the same reasoning as FAILED_STATUSES itself.
  const failed = isFailedStatus(status)
  const completion = completionKind(run)
  const pos = chainPosition(run)
  const tone: Tone = failed
    ? 'problem'
    : completion === 'stopped'
      ? 'attention'
      : completion === 'handoff'
        ? 'neutral'
        : 'good'
  const heading = memoryKilled
    ? 'Killed: memory limit'
    : failed
      ? 'Failed'
      : completion === 'stopped'
        ? 'Stopped'
        : completion === 'handoff'
          ? pos
            ? `Step ${pos.step}/${pos.total} done`
            : 'Step done'
          : 'Done'
  const body =
    memoryKilled && !summary
      ? 'The agent process exceeded its per-run memory limit and was stopped. Raise TF_RUN_MEMORY_LIMIT_MB if this run legitimately needs more.'
      : summary
  return (
    <div className="px-4 pb-1 pt-0.5">
      <div
        className={`mb-1 text-[10px] font-semibold uppercase tracking-wider ${TONE_TEXT[tone]}`}
        title={
          memoryKilled
            ? 'The sandbox enforces a per-run memory ceiling (TF_RUN_MEMORY_LIMIT_MB, default 4096 MB). This run exceeded it and was killed to protect the host.'
            : undefined
        }
      >
        {heading}
      </div>
      <p className="line-clamp-3 text-[12px] leading-relaxed text-text-secondary">{body}</p>
    </div>
  )
}

// chainStepStates maps a chain's runs to the progress-track states.
function chainStepStates(
  steps: Conversation[],
  currentRunID: string,
  currentStepIndex?: number,
): StepState[] {
  return steps.map((s, i) => {
    if (isActiveStatus(s.Status)) return 'active'
    if (s.Status === 'completed') return 'done'
    if (isFailedStatus(s.Status)) return 'failed'
    if (s.ID === currentRunID || i === currentStepIndex) return 'current'
    return 'pending'
  })
}
