import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import type { AgentMessage, AgentRun, Task, ToolCall } from '../types'
import { useOrgHref } from '../hooks/useOrgHref'
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
import TakeoverModal, { type TakeoverInfo } from './TakeoverModal'
import YieldModal, { type YieldRequest } from './YieldModal'
import { AttentionRow, CardPlane, EventTag, HudHeader, SourceTag } from './board/cardChrome'
import { compactNum, runGlow, runTone, TONE_TEXT, type StepState } from './board/cardStyle'

interface Props {
  task: Task
  run: AgentRun
  chainSteps?: AgentRun[]
  messages: AgentMessage[]
  onRequeue?: () => void
  onReview?: () => void
  // SKY-330: caller-supplied assignee picker, rendered in the header cluster.
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
  onRequeue,
  onReview,
  assigneeSlot,
}: Props) {
  const orgHref = useOrgHref()
  const scrollRef = useRef<HTMLDivElement>(null)
  const [now, setNow] = useState(() => Date.now())
  const [takeoverInfo, setTakeoverInfo] = useState<TakeoverInfo | null>(null)
  const [takeoverPending, setTakeoverPending] = useState(false)
  // While a takeover is pending we cover the activity log with a pulse; clicking
  // it dismisses so the user can still scroll the log. Reset on each attempt.
  const [pendingOverlayDismissed, setPendingOverlayDismissed] = useState(false)

  const isActive = isActiveRun(run)
  const isAwaiting = run.Status === 'awaiting_input'

  // Locate the open yield_request (latest) so the attention row + modal render.
  const openYield = useMemo<{ request: YieldRequest; messageID: number } | null>(() => {
    if (!isAwaiting) return null
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i].Subtype === 'yield_request') {
        try {
          return {
            request: JSON.parse(messages[i].Content) as YieldRequest,
            messageID: messages[i].ID,
          }
        } catch {
          return null
        }
      }
    }
    return null
  }, [messages, isAwaiting])
  const openYieldRequest = openYield?.request ?? null
  const openYieldMessageID = openYield?.messageID ?? 0
  const [yieldModalOpen, setYieldModalOpen] = useState(false)
  // Takeover only makes sense once the agent is actually running with a session.
  const canTakeOver = run.Status === 'running' && !!run.SessionID

  // Auto-scroll the log to the bottom when new messages arrive.
  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [messages.length])

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
  const isPendingApproval = run.Status === 'pending_approval'
  const isFailed = run.Status === 'failed'
  const isCancelled = run.Status === 'cancelled'
  // task_unsolvable: the agent's self-reported "can't finish without a human."
  // Functionally a failure from the user's POV, so it gets the same
  // Return-to-queue affordance.
  const isUnsolvable = run.Status === 'task_unsolvable'
  // A "held" takeover has a live worktree on disk; releasing tears it down.
  const isHeldTakeover = run.Status === 'taken_over' && !!run.WorktreePath
  const [releasePending, setReleasePending] = useState(false)

  const tone = runTone(run)
  const glow = runGlow(run)
  const stats = computeStats(messages, run)

  const hasChain = !!chainSteps && chainSteps.length > 1
  const stepStates: StepState[] = useMemo(
    () =>
      hasChain ? chainStepStates(chainSteps!, run.ID, run.blueprint_step_index ?? undefined) : [],
    [hasChain, chainSteps, run.ID, run.blueprint_step_index],
  )
  const stepLabel =
    hasChain && (run.blueprint_step_index ?? -1) >= 0
      ? `Step ${(run.blueprint_step_index ?? 0) + 1} / ${chainSteps!.length}`
      : null

  return (
    <div className="relative">
      <CardPlane tone={tone} steps={hasChain ? stepStates : undefined} glow={glow}>
        <TakeoverModal info={takeoverInfo} onClose={() => setTakeoverInfo(null)} />
        {openYieldRequest && (
          <YieldModal
            key={openYieldMessageID}
            runID={run.ID}
            request={openYieldRequest}
            open={yieldModalOpen}
            onClose={() => setYieldModalOpen(false)}
          />
        )}

        {/* Header label plate */}
        <HudHeader>
          <div className="flex items-center justify-between gap-2">
            <div className="flex min-w-0 items-center gap-2.5">
              <SourceTag task={task} />
              <EventTag eventType={task.event_type} />
              <RequestedReviewerBadge task={task} />
            </div>
            <div className="flex shrink-0 items-center gap-2">
              {assigneeSlot}
              <span className="inline-flex items-center gap-1.5 font-mono text-[11px] tabular-nums text-text-tertiary/80">
                {isActive && (
                  <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-delegate" />
                )}
                {elapsed}
              </span>
              <Link
                to={orgHref(`/board/runs/${run.ID}`)}
                aria-label="Expand run details"
                title="Open full session view"
                className="text-text-tertiary transition-colors hover:text-text-primary"
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
              {(canTakeOver || takeoverPending) && (
                <button
                  disabled={takeoverPending}
                  onClick={async () => {
                    setPendingOverlayDismissed(false)
                    setTakeoverPending(true)
                    try {
                      const res = await fetch(`/api/agent/runs/${run.ID}/takeover`, {
                        method: 'POST',
                      })
                      if (!res.ok) {
                        toast.error(await readError(res, 'Failed to take over run'))
                        return
                      }
                      const data = (await res.json()) as TakeoverInfo
                      setTakeoverInfo(data)
                    } catch (err) {
                      toast.error(`Failed to take over run: ${(err as Error).message}`)
                    } finally {
                      setTakeoverPending(false)
                    }
                  }}
                  className="inline-flex items-center gap-1.5 rounded-md px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-accent transition-colors hover:bg-accent/10 disabled:cursor-wait"
                  title={
                    takeoverPending
                      ? 'Stopping the headless session and preparing your takeover dir…'
                      : 'Stop the headless run, link a worktree at your takeover dir, and resume the session in your terminal'
                  }
                >
                  {takeoverPending ? (
                    <>
                      <Spinner />
                      <span>Taking over…</span>
                    </>
                  ) : (
                    <span>Take over</span>
                  )}
                </button>
              )}
              {isActive && (
                <button
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
                  className="text-dismiss/40 transition-colors hover:text-dismiss"
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
            {stepLabel && (
              <>
                <span aria-hidden className="text-text-tertiary/50">
                  ·
                </span>
                <span className="shrink-0 font-mono uppercase tracking-wider">{stepLabel}</span>
              </>
            )}
          </div>
        </div>

        {/* Transcript shelf: the activity stream with a faded gutter rail and a
            soft mask top + bottom, so the log emerges from the mist rather than
            sitting in a boxed-in widget. */}
        <div className="relative mx-4 mb-1">
          <div
            aria-hidden
            className="absolute bottom-0 left-0 top-0 w-px"
            style={{
              background:
                'linear-gradient(to bottom, transparent, var(--color-border-subtle) 12px, var(--color-border-subtle) calc(100% - 12px), transparent)',
            }}
          />
          <div
            ref={scrollRef}
            className={`max-h-[190px] overflow-y-auto pl-3 transition-[filter,opacity] ${
              takeoverPending && !pendingOverlayDismissed
                ? 'pointer-events-none opacity-50 grayscale'
                : ''
            }`}
            style={{ maskImage: SHELF_MASK, WebkitMaskImage: SHELF_MASK }}
          >
            {messages.length === 0 && isActive && (
              <div className="py-2 text-[12px] text-text-tertiary">Waiting for agent…</div>
            )}
            {renderActivityLog(messages, isActive, run)}
          </div>
          {takeoverPending && !pendingOverlayDismissed && (
            <button
              type="button"
              onClick={() => setPendingOverlayDismissed(true)}
              className="group absolute inset-0 flex items-center justify-center rounded-xl bg-surface-raised/40 backdrop-blur-[1px]"
              aria-label="Dismiss to view agent messages while takeover finishes"
            >
              <span className="inline-flex items-center gap-2 rounded-full border border-border-glass bg-surface-raised/90 px-3 py-1.5 text-[11px] font-semibold uppercase tracking-wider text-accent shadow-sm">
                <span className="inline-block h-2 w-2 animate-pulse rounded-full bg-accent" />
                Loading takeover
                <span className="font-medium normal-case tracking-normal text-text-tertiary opacity-0 transition-opacity group-hover:opacity-100">
                  · click to view log
                </span>
              </span>
            </button>
          )}
        </div>

        {/* Attention rows — your move, in the canonical toned pattern. */}
        {isAwaiting && openYieldRequest && (
          <div className="mx-4 mb-2">
            <AttentionRow
              kicker="Agent waiting for response"
              message={openYieldRequest.message}
              action="Respond"
              onClick={() => setYieldModalOpen(true)}
            />
          </div>
        )}
        {isPendingApproval && onReview && (
          <div className="mx-4 mb-2">
            <AttentionRow
              kicker={run.pending_kind === 'pr' ? 'PR ready to open' : 'Review ready'}
              action={run.pending_kind === 'pr' ? 'Open PR' : 'Review'}
              onClick={onReview}
            />
          </div>
        )}

        {/* Footer */}
        <div className="flex items-center justify-between pb-3.5 pl-4 pr-4">
          <div className="flex items-center gap-3 font-mono text-[11px] tabular-nums tracking-wide text-text-tertiary/80">
            {stats.comments > 0 && <span>{stats.comments} comments</span>}
            {stats.tokens > 0 && <span>{compactNum(stats.tokens)} tokens</span>}
            {run.TotalCostUSD != null && run.TotalCostUSD > 0 && (
              <span>${run.TotalCostUSD.toFixed(3)}</span>
            )}
          </div>

          <div className="flex items-center gap-3">
            {(isFailed || isCancelled || isUnsolvable || isPendingApproval) && onRequeue && (
              <button
                onClick={onRequeue}
                className="text-[12px] font-medium text-text-tertiary transition-colors hover:text-text-secondary"
              >
                Return to queue
              </button>
            )}
            {isHeldTakeover && (
              <button
                disabled={releasePending}
                onClick={async () => {
                  if (!confirm('Release this takeover? The worktree dir will be deleted.')) return
                  setReleasePending(true)
                  try {
                    const res = await fetch(`/api/agent/runs/${run.ID}/release`, { method: 'POST' })
                    if (!res.ok) {
                      toast.error(await readError(res, 'Failed to release takeover'))
                    }
                  } catch (err) {
                    toast.error(`Failed to release takeover: ${(err as Error).message}`)
                  } finally {
                    setReleasePending(false)
                  }
                }}
                className="text-[12px] font-medium text-text-tertiary transition-colors hover:text-dismiss disabled:cursor-wait disabled:opacity-50"
                title={
                  releasePending
                    ? 'Releasing the takeover dir…'
                    : 'Tear down the takeover worktree so the next delegated run on this PR can run'
                }
              >
                {releasePending ? 'Releasing…' : 'Release worktree'}
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

const SHELF_MASK =
  'linear-gradient(to bottom, transparent 0, #000 12px, #000 calc(100% - 12px), transparent 100%)'

// chainStepStates maps a chain's runs to the spine-notch states.
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

// Build a paired activity log: assistant turns with their tool results nested
// underneath. Borderless rows (no per-row rule) — the stream reads as the
// card's own depth under the shelf mask.
function renderActivityLog(messages: AgentMessage[], isActive: boolean, run: AgentRun) {
  const elements: React.ReactNode[] = []

  const toolResults = new Map<string, AgentMessage>()
  for (const msg of messages) {
    if (msg.Role === 'tool' && msg.ToolCallID) {
      toolResults.set(msg.ToolCallID, msg)
    }
  }

  for (const msg of messages) {
    const time = new Date(msg.CreatedAt).toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })

    // SKY-139: yield_request / yield_response render as compact Q+A rows.
    if (msg.Subtype === 'yield_request') {
      let parsedMessage = ''
      try {
        const req = JSON.parse(msg.Content) as { message?: string }
        parsedMessage = req.message || ''
      } catch {
        parsedMessage = msg.Content
      }
      elements.push(
        <div key={`yreq-${msg.ID}`} className="flex items-start gap-2 py-1 text-[12px]">
          <span className="mt-0.5 shrink-0 font-mono text-[10px] text-text-tertiary opacity-60">
            {time}
          </span>
          <span className="leading-snug text-snooze">❓ {parsedMessage}</span>
        </div>,
      )
      continue
    }
    if (msg.Subtype === 'yield_response') {
      elements.push(
        <div key={`yres-${msg.ID}`} className="flex items-start gap-2 py-1 text-[12px]">
          <span className="mt-0.5 shrink-0 font-mono text-[10px] text-text-tertiary opacity-60">
            {time}
          </span>
          <span className="leading-snug text-text-secondary">
            ↩ <span className="font-medium text-text-primary">{msg.Content}</span>
          </span>
        </div>,
      )
      continue
    }

    if (msg.Role !== 'assistant') continue

    // Skip the raw JSON completion message (the agent's structured output).
    if (msg.Content && msg.Content.trimStart().startsWith('{"status":')) continue

    if (msg.Content) {
      const text = msg.Content.length > 150 ? msg.Content.slice(0, 147) + '...' : msg.Content
      elements.push(
        <div key={`text-${msg.ID}`} className="flex items-start gap-2 py-1 text-[12px]">
          <span className="mt-0.5 shrink-0 font-mono text-[10px] text-text-tertiary opacity-60">
            {time}
          </span>
          <span className="leading-snug text-text-secondary">{text}</span>
        </div>,
      )
    }

    if (msg.ToolCalls?.length) {
      for (const tc of msg.ToolCalls) {
        const label = formatToolCall(tc.name, tc.input)
        const result = toolResults.get(tc.id)

        elements.push(
          <div key={`tc-${tc.id}`} className="py-1">
            <div className="flex items-start gap-2 text-[12px]">
              <span className="mt-0.5 shrink-0 font-mono text-[10px] text-text-tertiary opacity-60">
                {time}
              </span>
              <span className="leading-snug text-text-secondary">{label}</span>
            </div>
            {result ? (
              <div
                className={`ml-[4.25rem] mt-1 rounded px-2.5 py-1 text-[11px] leading-snug ${
                  result.IsError
                    ? 'bg-dismiss/5 text-dismiss'
                    : 'bg-black/[0.02] text-text-tertiary'
                }`}
              >
                {formatToolResult(tc, result)}
              </div>
            ) : isActive ? (
              <div className="ml-[4.25rem] mt-1 text-[11px] text-text-tertiary">
                <span className="mr-1.5 inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-delegate" />
                Running…
              </div>
            ) : null}
          </div>,
        )
      }
    }
  }

  // Terminal outcome — a borderless block under a hairline rule, not a card.
  if (run.ResultSummary && !isActive) {
    const summaryTone =
      run.Status === 'failed' || run.Status === 'cancelled'
        ? 'problem'
        : run.Status === 'task_unsolvable'
          ? 'attention'
          : 'good'
    const heading =
      run.Status === 'cancelled'
        ? '◼ Cancelled'
        : run.Status === 'failed'
          ? '✗ Failed'
          : run.Status === 'task_unsolvable'
            ? '⊘ Unsolvable'
            : '✓ Done'
    elements.push(
      <div key="result-summary" className="mt-2.5 border-t border-border-subtle/70 pb-1 pt-2.5">
        <div className={`mb-1 text-[11px] font-semibold tracking-wide ${TONE_TEXT[summaryTone]}`}>
          {heading}
        </div>
        <p className="text-[12px] leading-relaxed text-text-secondary">{run.ResultSummary}</p>
      </div>,
    )
  }

  return elements
}

function formatToolCall(name: string, input: Record<string, unknown>): string {
  if (name === 'Bash') {
    const cmd = String(input.command || '')
    if (cmd.includes('triagefactory exec gh pr view')) return 'Fetching PR details'
    if (cmd.includes('triagefactory exec gh pr diff') && cmd.includes('--file'))
      return `Reading diff: ${extractFlag(cmd, '--file')}`
    if (cmd.includes('triagefactory exec gh pr diff')) return 'Reading full diff'
    if (cmd.includes('triagefactory exec gh pr files')) return 'Listing changed files'
    if (cmd.includes('triagefactory exec gh pr review-view')) return 'Expanding previous review'
    if (cmd.includes('triagefactory exec gh pr start-review')) return 'Starting review'
    if (cmd.includes('triagefactory exec gh pr add-review-comment')) {
      const file = extractFlag(cmd, '--file')
      return file ? `Adding comment on ${file}` : 'Adding review comment'
    }
    if (cmd.includes('triagefactory exec gh pr submit-review')) {
      const event = extractFlag(cmd, '--event')
      return `Submitting review (${event || 'comment'})`
    }
    if (cmd.includes('triagefactory exec gh pr comment-list-pending'))
      return 'Reviewing pending comments'
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

function formatToolResult(tc: ToolCall, result: AgentMessage): string {
  if (result.IsError) {
    const text = result.Content || 'Unknown error'
    return text.length > 120 ? text.slice(0, 117) + '...' : text
  }

  if (!result.Content) return '✓'

  try {
    const data = JSON.parse(result.Content)

    if (data.github_review_id != null) {
      const event = (data.event || 'comment').toLowerCase()
      const count = data.comments_posted || 0
      return `${event} review posted — ${count} comment${count !== 1 ? 's' : ''}`
    }
    if (data.comment_id && data.review_id && data.status === 'pending_local') {
      return 'Comment added to pending review'
    }
    if (data.review_id && data.status === 'pending_local' && data.files != null) {
      return `Review started — ${data.files} files in diff`
    }
    if (Array.isArray(data)) {
      return `${data.length} pending comment${data.length !== 1 ? 's' : ''}`
    }
    if (data.number && data.title) {
      const reviews = data.reviews?.length || 0
      const comments = data.comments?.length || 0
      return `PR #${data.number}: ${reviews} review${reviews !== 1 ? 's' : ''}, ${comments} comment${comments !== 1 ? 's' : ''}`
    }
    if (Array.isArray(data) && data[0]?.filename) {
      return `${data.length} file${data.length !== 1 ? 's' : ''} changed`
    }
    if (data.id && data.state && data.comments) {
      return `${data.author}: ${data.state.toLowerCase()} — ${data.comments.length} comment${data.comments.length !== 1 ? 's' : ''}`
    }
    if (data.ok) return '✓'
    if (data.scope) return `✓ (${data.scope})`
  } catch {
    // Not JSON
  }

  const toolName = tc.name
  if (toolName === 'Read' || toolName === 'Glob' || toolName === 'Grep') {
    const lines = result.Content.split('\n').length
    return `${lines} line${lines !== 1 ? 's' : ''}`
  }

  if (result.Content.startsWith('diff --git')) {
    const files = result.Content.split('diff --git').length - 1
    return `Diff loaded — ${files} file${files !== 1 ? 's' : ''}`
  }

  const text = result.Content
  return text.length > 80 ? text.slice(0, 77) + '...' : text
}

// 12px spinner sized to match the Take over button's text.
function Spinner() {
  return (
    <svg
      width="11"
      height="11"
      viewBox="0 0 16 16"
      fill="none"
      className="animate-spin text-accent"
      aria-hidden
    >
      <circle cx="8" cy="8" r="6" stroke="currentColor" strokeOpacity="0.25" strokeWidth="2" />
      <path d="M8 2a6 6 0 0 1 6 6" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  )
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
