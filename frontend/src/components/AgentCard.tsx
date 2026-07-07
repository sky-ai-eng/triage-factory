import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import * as Popover from '@radix-ui/react-popover'
import type { AgentMessage, AgentRun, Task } from '../types'
import { useOrgHref } from '../hooks/useOrgHref'
import ArtifactList from './ArtifactList'
import RequestedReviewerBadge from './RequestedReviewerBadge'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'
import {
  formatDurationMs,
  formatElapsed,
  isActiveRun,
  isActiveStatus,
  isFailedStatus,
} from '../lib/runStatus'
import { AttentionRow, CardPlane, EventTag, HudHeader, SourceTag } from './board/cardChrome'
import { compactNum, runGlow, TONE_TEXT, type Glow, type StepState } from './board/cardStyle'
import { PermissionPrompt } from './permissions/PermissionPrompt'
import type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'
import {
  approvalAction,
  approvalCounts,
  approvalKicker,
  hasUnresolvedArtifacts,
} from '../lib/approval'

interface Props {
  task: Task
  run: AgentRun
  chainSteps?: AgentRun[]
  messages: AgentMessage[]
  // Unanswered tool-permission prompts for this run, head-first. When non-empty
  // the card renders an inline Allow/Deny control and takes the attention tone.
  pendingPermissions?: PendingPermission[]
  onResolvePermission?: (requestID: string, decision: PermissionDecisionInput) => Promise<void>
  onRequeue?: () => void
  onReview?: () => void
  // Open a PR/review artifact's approval overlay by id, from the footer's
  // artifacts popover (TFAC-470). Distinct from onReview, which fires the parked
  // run's single gating overlay; this addresses any artifact in the full set.
  onOpenArtifact?: (kind: 'review' | 'pr', artifactId: string) => void
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
  messages,
  pendingPermissions,
  onResolvePermission,
  onRequeue,
  onReview,
  onOpenArtifact,
  assigneeSlot,
}: Props) {
  const orgHref = useOrgHref()
  const [now, setNow] = useState(() => Date.now())

  const isActive = isActiveRun(run)

  // Tick once per second while active so the elapsed display updates live.
  useEffect(() => {
    if (!isActive) return
    const interval = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(interval)
  }, [isActive])

  const elapsed =
    !isActive && run.DurationMs != null
      ? formatDurationMs(run.DurationMs)
      : formatElapsed(run.StartedAt, now)
  // Approval is derived from the unresolved-artifact set (draft PRs + ready
  // reviews), not a `pending_approval` run status — a card needs the user
  // whenever has_unresolved_artifacts is true, live or terminal.
  const needsApproval = hasUnresolvedArtifacts(run)
  const approval = approvalCounts(run)
  const isFailed = run.Status === 'failed'
  const isCancelled = run.Status === 'cancelled'
  // task_unsolvable: the agent's self-reported "can't finish without a human."
  // Functionally a failure from the user's POV, so it gets the same
  // Return-to-queue affordance.
  const isUnsolvable = run.Status === 'task_unsolvable'
  // A terminal run is settled — show its outcome, not a live feed.
  const isTerminal = isFailed || isCancelled || isUnsolvable || run.Status === 'completed'

  // A parked tool-permission prompt OR an unresolved artifact set is the card's
  // "needs you" moment — it takes the steady amber attention glow so it can't be
  // missed in the column.
  const pending = pendingPermissions ?? []
  const hasPending = pending.length > 0
  const glow: Glow | null =
    hasPending || needsApproval ? { tone: 'attention', breathing: false } : runGlow(run)
  const stats = computeStats(messages, run)

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
              <span className="inline-flex items-center gap-1.5 font-mono text-[11px] leading-none tabular-nums text-text-tertiary/80">
                {isActive && (
                  <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-delegate" />
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
                        const res = await fetch(`/api/agent/runs/${run.ID}/cancel`, {
                          method: 'POST',
                        })
                        if (!res.ok) toast.error(await readError(res, 'Failed to cancel run'))
                      } catch (err) {
                        toast.error(`Failed to cancel run: ${(err as Error).message}`)
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

        {/* Activity — a terminal run shows its outcome; a live run shows a
            flush, borderless feed of one-liners (no window-into-the-agent; the
            expanded view is for that). A memory-limit kill has no
            ResultSummary (infra failures write run_messages, not a summary)
            but still gets the ResultBlock — its kind carries the copy. */}
        {isTerminal && (run.ResultSummary || run.FailureKind === 'memory_limit') ? (
          <ResultBlock
            status={run.Status}
            summary={run.ResultSummary}
            failureKind={run.FailureKind}
          />
        ) : (
          <LiveFeed messages={messages} isActive={isActive} />
        )}

        {/* Inline tool-permission prompt — the parked turn's Allow/Deny,
            answerable without opening the run. Head of queue + "N more". */}
        {hasPending && (
          <div className="mx-4 mb-2">
            <PermissionPrompt
              key={pending[0].request_id}
              prompt={pending[0]}
              remaining={pending.length - 1}
              worktree={run.WorktreePath}
              onResolve={onResolvePermission}
              compact
            />
          </div>
        )}

        {/* Attention row — your move, in the canonical toned pattern. Count-aware
            over the unresolved set; opening it raises the approval surface. */}
        {needsApproval && onReview && (
          <div className="mx-4 mb-2">
            <AttentionRow
              kicker={approvalKicker(approval)}
              action={approvalAction(approval)}
              onClick={onReview}
            />
          </div>
        )}

        {/* Footer */}
        <div className="flex items-center justify-between pb-3.5 pl-4 pr-4">
          <div className="flex items-center gap-3 font-mono text-[11px] tabular-nums tracking-wide text-text-tertiary/80">
            {run.actor_agent_name && (
              <span title="The bot that executed this run">Ran as {run.actor_agent_name}</span>
            )}
            {stats.comments > 0 && <span>{stats.comments} comments</span>}
            {stats.tokens > 0 && <span>{compactNum(stats.tokens)} tokens</span>}
            {run.TotalCostUSD != null && run.TotalCostUSD > 0 && (
              <span>${run.TotalCostUSD.toFixed(3)}</span>
            )}
            <ArtifactsAffordance run={run} onOpenArtifact={onOpenArtifact} />
          </div>

          <div className="flex items-center gap-3">
            {(isFailed || isCancelled || isUnsolvable || needsApproval) && onRequeue && (
              <button
                onClick={onRequeue}
                className="text-[12px] font-medium text-text-tertiary transition-colors hover:text-text-secondary"
              >
                Return to queue
              </button>
            )}
            <a
              href={task.source_url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-[12px] font-semibold text-accent transition-colors hover:text-accent/70"
            >
              Open
            </a>
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
// run produced (the count comes free with the run, TFAC-465). Hidden at 0.
// Clicking opens a lightweight popover that lazy-fetches the list (ArtifactList
// only mounts when the popover does); PR/review rows reuse the board's approval
// overlays via onOpenArtifact, the rest link out.
function ArtifactsAffordance({
  run,
  onOpenArtifact,
}: {
  run: AgentRun
  onOpenArtifact?: (kind: 'review' | 'pr', artifactId: string) => void
}) {
  const [open, setOpen] = useState(false)
  const count = run.artifact_count ?? 0
  if (count <= 0) return null
  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
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
            onOpenApproval={(kind, id) => {
              setOpen(false)
              onOpenArtifact?.(kind, id)
            }}
          />
          <Popover.Arrow className="fill-surface-raised" />
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

// The feed fades in at its top edge so older lines dissolve as new ones arrive.
const FEED_MASK = 'linear-gradient(to bottom, transparent 0, #000 22px, #000 100%)'

interface FeedLine {
  id: string
  time: string
  text: string
}

// feedLines flattens the transcript into compact one-liners — the agent's
// actions (tool calls) and its prose turns — for the card's live ticker. The
// full, nested transcript lives in the expanded run view.
function feedLines(messages: AgentMessage[]): FeedLine[] {
  const out: FeedLine[] = []
  for (const msg of messages) {
    const time = new Date(msg.CreatedAt).toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    })
    // Operator steers show on the ticker too — the card should reflect that
    // someone redirected the run.
    if (msg.Role === 'user' && msg.Content) {
      out.push({ id: `u-${msg.ID}`, time, text: `you: ${clip(msg.Content, 64)}` })
      continue
    }
    if (msg.Role !== 'assistant') continue
    // Skip the raw JSON completion message (the agent's structured output).
    if (msg.Content && msg.Content.trimStart().startsWith('{"status":')) continue
    // Reasoning stays off the ticker — it's a verbose stream; the expanded
    // run view renders it under its own THINKING rows.
    if (msg.Subtype === 'thinking') continue
    if (msg.Content) out.push({ id: `txt-${msg.ID}`, time, text: clip(msg.Content, 70) })
    if (msg.ToolCalls?.length) {
      for (const tc of msg.ToolCalls) {
        out.push({ id: `tc-${tc.id}`, time, text: formatToolCall(tc.name, tc.input) })
      }
    }
  }
  return out
}

function clip(s: string, n: number): string {
  const t = s.replace(/\s+/g, ' ').trim()
  return t.length > n ? t.slice(0, n - 1) + '…' : t
}

// LiveFeed is the flush, borderless activity ticker: a few of the most recent
// one-liners in monospace, auto-advancing to the newest, top-masked so older
// lines dissolve upward. No box, no scrollbar — it's part of the card.
function LiveFeed({ messages, isActive }: { messages: AgentMessage[]; isActive: boolean }) {
  const lines = useMemo(() => feedLines(messages), [messages])

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
function ResultBlock({
  status,
  summary,
  failureKind,
}: {
  status: string
  summary: string
  failureKind?: string
}) {
  const memoryKilled = status === 'failed' && failureKind === 'memory_limit'
  const tone =
    status === 'failed' || status === 'cancelled'
      ? 'problem'
      : status === 'task_unsolvable'
        ? 'attention'
        : 'good'
  const heading =
    status === 'cancelled'
      ? 'Cancelled'
      : status === 'failed'
        ? memoryKilled
          ? 'Killed: memory limit'
          : 'Failed'
        : status === 'task_unsolvable'
          ? 'Unsolvable'
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
  steps: AgentRun[],
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

function formatToolCall(name: string, input: Record<string, unknown>): string {
  if (name === 'Bash') {
    // Prefer the agent-authored description — the human-readable intent.
    // The curated exec mappings below cover description-less internal calls.
    const desc = String(input.description || '')
    if (desc) return desc
    const cmd = String(input.command || '')
    if (cmd.includes('triagefactory exec gh pr view')) return 'Fetching PR details'
    if (cmd.includes('triagefactory exec gh pr diff') && cmd.includes('--file'))
      return `Reading diff: ${extractFlag(cmd, '--file')}`
    if (cmd.includes('triagefactory exec gh pr diff')) return 'Reading full diff'
    if (cmd.includes('triagefactory exec gh pr files')) return 'Listing changed files'
    if (cmd.includes('triagefactory exec gh pr review-view')) return 'Expanding previous review'
    if (cmd.includes('triagefactory exec gh pr start-review'))
      return cmd.includes('--fresh') ? 'Restarting review' : 'Starting review'
    if (cmd.includes('triagefactory exec gh pr add-review-comment')) {
      const file = extractFlag(cmd, '--file')
      return file ? `Adding comment on ${file}` : 'Adding review comment'
    }
    // finalize-review (renamed from submit-review): hands the drafted review to
    // human approval — it does not submit to GitHub, so the label says "Finalizing".
    if (cmd.includes('triagefactory exec gh pr finalize-review')) {
      const event = extractFlag(cmd, '--event')
      return `Finalizing review (${event || 'comment'})`
    }
    if (cmd.includes('triagefactory exec gh pr comment-list-pending'))
      return 'Reviewing pending comments'
    if (cmd.includes('triagefactory exec gh pr comment-update')) return 'Editing comment'
    if (cmd.includes('triagefactory exec gh pr comment-delete')) return 'Deleting comment'
    if (cmd.includes('triagefactory exec gh pr add-comment')) return 'Adding comment'
    if (cmd.includes('triagefactory exec'))
      return `Running: ${cmd.split('triagefactory exec ')[1]?.slice(0, 60)}`
    return `Running command`
  }
  if (name === 'Read') return `Reading ${basename(String(input.file_path || ''))}`
  if (name === 'Glob') return `Searching for ${String(input.pattern || 'files')}`
  if (name === 'Grep') return `Searching for "${String(input.pattern || '').slice(0, 40)}"`
  return `${name}`
}

function extractFlag(cmd: string, flag: string): string {
  const parts = cmd.split(/\s+/)
  const idx = parts.indexOf(flag)
  if (idx >= 0 && idx + 1 < parts.length) return parts[idx + 1]
  return ''
}

function basename(path: string): string {
  const parts = path.split('/')
  return parts[parts.length - 1] || path
}

function computeStats(messages: AgentMessage[], _run: AgentRun) {
  let comments = 0
  let tokens = 0

  for (const msg of messages) {
    if (msg.OutputTokens) tokens += msg.OutputTokens
    if (msg.InputTokens) tokens += msg.InputTokens

    if (msg.Role === 'assistant' && msg.Subtype === 'tool_use' && msg.ToolCalls?.length) {
      const cmd = String(msg.ToolCalls[0].input?.command || '')
      if (cmd.includes('add-review-comment') || cmd.includes('add-comment')) {
        comments++
      }
    }
  }

  return { comments, tokens }
}
