import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import type { AgentMessage, AgentRun, Task, ToolCall } from '../types'
import { useOrgHref } from '../hooks/useOrgHref'
import SourceBadge from './SourceBadge'
import RequestedReviewerBadge from './RequestedReviewerBadge'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'
import { formatDurationMs, formatElapsed, isActiveRun, statusDisplay } from '../lib/runStatus'
import ChainStepsRail from './ChainStepsRail'
import TakeoverModal, { type TakeoverInfo } from './TakeoverModal'
import YieldModal, { type YieldRequest } from './YieldModal'

interface Props {
  task: Task
  run: AgentRun
  chainSteps?: AgentRun[]
  messages: AgentMessage[]
  onRequeue?: () => void
  onReview?: () => void
  // SKY-330: caller-supplied assignee picker. Rendered inline at the
  // start of the header's right-side cluster (assignee | elapsed |
  // expand | takeover) so it shares the cluster's gap-2 spacing
  // instead of overlapping it via absolute positioning. Optional —
  // call sites outside the Board (factory views, etc.) omit it.
  assigneeSlot?: React.ReactNode
}

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
  // While a takeover is pending we cover the activity log with a "Loading
  // takeover" pulse. Clicking the overlay sets this flag so the user can
  // still scroll through agent messages while the backend works. Reset
  // every time we kick off a new takeover so a previous dismissal doesn't
  // bleed into the next attempt.
  const [pendingOverlayDismissed, setPendingOverlayDismissed] = useState(false)

  const isActive = isActiveRun(run)
  const isAwaiting = run.Status === 'awaiting_input'

  // Locate the open yield_request (latest one) so the attention row +
  // modal can render. Once status flips back to running the answered
  // request collapses into the transcript as a paired Q+A row.
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
  // Takeover only makes sense once the agent has actually started — earlier
  // phases either don't yet have a session_id (clone/fetch/worktree_created)
  // or are at the agent's startup boundary. Gating on session_id presence
  // also catches the brief window between agent_starting and the first
  // system/init event.
  const canTakeOver = run.Status === 'running' && !!run.SessionID

  // Auto-scroll to bottom when new messages arrive
  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [messages.length])

  // Tick `now` once per second while the run is active so the elapsed display
  // updates live. When the run ends, we stop ticking and display the fixed
  // duration derived from run.DurationMs below.
  useEffect(() => {
    if (!isActive) return
    const interval = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(interval)
  }, [isActive])

  const elapsed =
    !isActive && run.DurationMs != null
      ? formatDurationMs(run.DurationMs)
      : formatElapsed(run.StartedAt, now)
  const isFailed = run.Status === 'failed'
  const isCancelled = run.Status === 'cancelled'
  const isPendingApproval = run.Status === 'pending_approval'
  // task_unsolvable is the agent's self-reported "I tried, can't finish
  // without human action." Functionally a failure from the user's POV
  // (the run won't do anything more on its own), so it gets the same
  // Return-to-queue affordance as `failed` — colored as a warning rather
  // than a hard error since the agent exited cleanly. Without this
  // branch, the icon/color logic falls through to the success bucket
  // (green ✓) and the requeue button never renders, leaving the task
  // stranded in the Agent column with no visible way back.
  const isUnsolvable = run.Status === 'task_unsolvable'
  // A "held" takeover has a live worktree on disk; releasing it tears
  // that worktree down so the next delegated run on the same PR can
  // fetch into the branch ref. Released takeovers keep status='taken_over'
  // for audit but null out worktree_path — those should NOT show the
  // Release button (nothing to release).
  const isHeldTakeover = run.Status === 'taken_over' && !!run.WorktreePath
  const [releasePending, setReleasePending] = useState(false)

  const { color: statusColor, icon: statusIcon, label: statusLabel } = statusDisplay(run)

  const stats = computeStats(messages, run)

  return (
    <div className="bg-surface-raised backdrop-blur-xl border border-border-glass rounded-2xl overflow-hidden shadow-sm shadow-black/[0.03]">
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
      {/* Header */}
      <div className="px-5 pt-4 pb-3">
        <div className="flex items-center justify-between mb-2">
          <div className="flex items-center gap-2">
            <span className={`text-xs font-semibold ${statusColor}`}>
              {statusIcon} {statusLabel}
            </span>
            {isActive && (
              <span className="inline-block w-1.5 h-1.5 rounded-full bg-delegate animate-pulse" />
            )}
          </div>
          <div className="flex items-center gap-2">
            {assigneeSlot && <div className="shrink-0">{assigneeSlot}</div>}
            <span className="text-[11px] text-text-tertiary">{elapsed}</span>
            <Link
              to={orgHref(`/board/runs/${run.ID}`)}
              aria-label="Expand run details"
              title="Open full session view"
              className="text-text-tertiary hover:text-text-primary transition-colors"
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
                className="inline-flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider px-2 py-0.5 rounded-md text-accent bg-accent/10 hover:bg-accent/20 disabled:bg-accent/10 disabled:cursor-wait transition-colors"
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
                    const res = await fetch(`/api/agent/runs/${run.ID}/cancel`, { method: 'POST' })
                    if (!res.ok) toast.error(await readError(res, 'Failed to cancel run'))
                  } catch (err) {
                    toast.error(`Failed to cancel run: ${(err as Error).message}`)
                  }
                }}
                className="text-dismiss/40 hover:text-dismiss transition-colors"
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

        <h3 className="text-[14px] font-semibold text-text-primary leading-snug line-clamp-2 mb-1">
          {task.title}
        </h3>
        <div className="flex items-center gap-2 text-[11px] text-text-tertiary min-w-0">
          <SourceBadge task={task} />
          <span className="truncate">{task.source_id}</span>
          <RequestedReviewerBadge task={task} />
        </div>
      </div>

      {chainSteps && chainSteps.length > 1 && (
        <div className="mx-5 mb-3">
          <ChainStepsRail
            steps={chainSteps}
            currentRunID={run.ID}
            currentStepIndex={run.blueprint_step_index ?? undefined}
          />
        </div>
      )}

      {/* Activity log + optional pending-takeover overlay */}
      <div className="relative mx-3 mb-3">
        <div
          ref={scrollRef}
          className={`rounded-xl bg-black/[0.02] border border-border-subtle max-h-[200px] overflow-y-auto transition-[filter,opacity] ${takeoverPending && !pendingOverlayDismissed ? 'opacity-50 grayscale pointer-events-none' : ''}`}
        >
          {messages.length === 0 && isActive && (
            <div className="px-4 py-3 text-[12px] text-text-tertiary">Waiting for agent...</div>
          )}
          {renderActivityLog(messages, isActive, run)}
        </div>
        {isAwaiting && openYieldRequest && (
          <button
            type="button"
            onClick={() => setYieldModalOpen(true)}
            aria-label={`Respond to agent: ${openYieldRequest.message}`}
            aria-haspopup="dialog"
            className="mt-2 w-full flex items-center justify-between gap-3 px-3 py-2 rounded-xl border border-snooze/40 bg-snooze/10 hover:bg-snooze/20 transition-colors text-left"
          >
            <span className="flex items-start gap-2 min-w-0">
              <span
                className="shrink-0 mt-0.5 inline-block w-1.5 h-1.5 rounded-full bg-snooze animate-pulse"
                aria-hidden="true"
              />
              <span className="flex flex-col min-w-0">
                <span className="text-[10px] font-semibold uppercase tracking-wider text-snooze">
                  Agent waiting for response
                </span>
                <span className="text-[12px] text-text-primary leading-snug truncate">
                  {openYieldRequest.message}
                </span>
              </span>
            </span>
            <span className="shrink-0 text-[12px] font-semibold text-snooze" aria-hidden="true">
              Respond →
            </span>
          </button>
        )}
        {takeoverPending && !pendingOverlayDismissed && (
          <button
            type="button"
            onClick={() => setPendingOverlayDismissed(true)}
            className="absolute inset-0 rounded-xl flex items-center justify-center bg-surface-raised/40 backdrop-blur-[1px] cursor-pointer group"
            aria-label="Dismiss to view agent messages while takeover finishes"
          >
            <span className="inline-flex items-center gap-2 px-3 py-1.5 rounded-full bg-surface-raised/90 border border-border-glass shadow-sm text-[11px] font-semibold uppercase tracking-wider text-accent">
              <span className="inline-block w-2 h-2 rounded-full bg-accent animate-pulse" />
              Loading takeover
              <span className="text-text-tertiary normal-case font-medium tracking-normal opacity-0 group-hover:opacity-100 transition-opacity">
                · click to view log
              </span>
            </span>
          </button>
        )}
      </div>

      {/* Footer */}
      <div className="px-5 pb-4 flex items-center justify-between">
        <div className="flex items-center gap-3 text-[11px] text-text-tertiary">
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
              className="text-[12px] text-text-tertiary hover:text-text-primary font-medium transition-colors"
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
              className="text-[12px] text-text-tertiary hover:text-dismiss disabled:opacity-50 disabled:cursor-wait font-medium transition-colors"
              title={
                releasePending
                  ? 'Releasing the takeover dir…'
                  : 'Tear down the takeover worktree so the next delegated run on this PR can run'
              }
            >
              {releasePending ? 'Releasing…' : 'Release worktree'}
            </button>
          )}
          {isPendingApproval && onReview && (
            <button
              onClick={onReview}
              className="text-[12px] font-semibold text-snooze bg-snooze/10 hover:bg-snooze/20 px-3 py-1 rounded-lg transition-colors"
            >
              {run.pending_kind === 'pr' ? 'Open PR' : 'Review'}
            </button>
          )}
          <a
            href={task.source_url}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
          >
            Open
          </a>
        </div>
      </div>
    </div>
  )
}

// Build a paired activity log: assistant turns with their tool results nested underneath
function renderActivityLog(messages: AgentMessage[], isActive: boolean, run: AgentRun) {
  const elements: React.ReactNode[] = []

  // Build a map of tool_call_id → tool result message
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

    // SKY-139: yield_request / yield_response render as compact Q+A
    // rows in the transcript. The "current open yield" gets the
    // attention CTA outside the activity log; resolved yields stay
    // here as transcript history.
    if (msg.Subtype === 'yield_request') {
      let parsedMessage = ''
      try {
        const req = JSON.parse(msg.Content) as { message?: string }
        parsedMessage = req.message || ''
      } catch {
        parsedMessage = msg.Content
      }
      elements.push(
        <div
          key={`yreq-${msg.ID}`}
          className="flex items-start gap-2 px-4 py-1.5 text-[12px] border-b border-border-subtle/50"
        >
          <span className="shrink-0 mt-0.5 text-[10px] text-text-tertiary opacity-60 font-mono">
            {time}
          </span>
          <span className="text-snooze leading-snug">❓ {parsedMessage}</span>
        </div>,
      )
      continue
    }
    if (msg.Subtype === 'yield_response') {
      elements.push(
        <div
          key={`yres-${msg.ID}`}
          className="flex items-start gap-2 px-4 py-1.5 text-[12px] border-b border-border-subtle/50"
        >
          <span className="shrink-0 mt-0.5 text-[10px] text-text-tertiary opacity-60 font-mono">
            {time}
          </span>
          <span className="text-text-secondary leading-snug">
            ↩ <span className="font-medium text-text-primary">{msg.Content}</span>
          </span>
        </div>,
      )
      continue
    }

    if (msg.Role !== 'assistant') continue

    // Skip the raw JSON completion message (the agent's structured output)
    if (msg.Content && msg.Content.trimStart().startsWith('{"status":')) continue

    // Text content (if any)
    if (msg.Content) {
      const text = msg.Content.length > 150 ? msg.Content.slice(0, 147) + '...' : msg.Content
      elements.push(
        <div
          key={`text-${msg.ID}`}
          className="flex items-start gap-2 px-4 py-1.5 text-[12px] border-b border-border-subtle/50"
        >
          <span className="shrink-0 mt-0.5 text-[10px] text-text-tertiary opacity-60 font-mono">
            {time}
          </span>
          <span className="text-text-secondary leading-snug">{text}</span>
        </div>,
      )
    }

    // Tool calls with paired results
    if (msg.ToolCalls?.length) {
      for (const tc of msg.ToolCalls) {
        const label = formatToolCall(tc.name, tc.input)
        const result = toolResults.get(tc.id)

        elements.push(
          <div key={`tc-${tc.id}`} className="border-b border-border-subtle/50">
            <div className="flex items-start gap-2 px-4 py-1.5 text-[12px]">
              <span className="shrink-0 mt-0.5 text-[10px] text-text-tertiary opacity-60 font-mono">
                {time}
              </span>
              <span className="text-text-secondary leading-snug">{label}</span>
            </div>
            {result ? (
              <div
                className={`ml-[4.5rem] mr-4 mb-1.5 px-2.5 py-1 rounded text-[11px] leading-snug ${
                  result.IsError
                    ? 'bg-dismiss/5 text-dismiss'
                    : 'bg-black/[0.02] text-text-tertiary'
                }`}
              >
                {formatToolResult(tc, result)}
              </div>
            ) : isActive ? (
              <div className="ml-[4.5rem] mr-4 mb-1.5 px-2.5 py-1 text-[11px] text-text-tertiary">
                <span className="inline-block w-1.5 h-1.5 rounded-full bg-delegate animate-pulse mr-1.5" />
                Running...
              </div>
            ) : null}
          </div>,
        )
      }
    }
  }

  // Append result as a frosted summary card
  if (run.ResultSummary && !isActive) {
    const isFailed = run.Status === 'failed' || run.Status === 'cancelled'
    const isUnsolvable = run.Status === 'task_unsolvable'
    elements.push(
      <div
        key="result-summary"
        className="mx-2 my-2 rounded-xl backdrop-blur-sm bg-white/50 border border-border-glass p-3.5"
      >
        <div className="mb-2">
          <span
            className={`text-[11px] font-semibold tracking-wide ${
              isFailed ? 'text-dismiss' : isUnsolvable ? 'text-snooze' : 'text-text-primary'
            }`}
          >
            {run.Status === 'cancelled'
              ? '◼ Cancelled'
              : isFailed
                ? '✗ Failed'
                : isUnsolvable
                  ? '⊘ Unsolvable'
                  : '✓ Done'}
          </span>
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

  // Try to parse as JSON for structured results
  try {
    const data = JSON.parse(result.Content)

    // submit-review result
    if (data.github_review_id != null) {
      const event = (data.event || 'comment').toLowerCase()
      const count = data.comments_posted || 0
      return `${event} review posted — ${count} comment${count !== 1 ? 's' : ''}`
    }

    // add-review-comment result
    if (data.comment_id && data.review_id && data.status === 'pending_local') {
      return 'Comment added to pending review'
    }

    // start-review result
    if (data.review_id && data.status === 'pending_local' && data.files != null) {
      return `Review started — ${data.files} files in diff`
    }

    // comment-list-pending (array)
    if (Array.isArray(data)) {
      return `${data.length} pending comment${data.length !== 1 ? 's' : ''}`
    }

    // pr view result
    if (data.number && data.title) {
      const reviews = data.reviews?.length || 0
      const comments = data.comments?.length || 0
      return `PR #${data.number}: ${reviews} review${reviews !== 1 ? 's' : ''}, ${comments} comment${comments !== 1 ? 's' : ''}`
    }

    // pr files result (array of file objects)
    if (Array.isArray(data) && data[0]?.filename) {
      return `${data.length} file${data.length !== 1 ? 's' : ''} changed`
    }

    // review-view result
    if (data.id && data.state && data.comments) {
      return `${data.author}: ${data.state.toLowerCase()} — ${data.comments.length} comment${data.comments.length !== 1 ? 's' : ''}`
    }

    // Generic ok
    if (data.ok) return '✓'

    // comment-update / comment-delete
    if (data.scope) return `✓ (${data.scope})`
  } catch {
    // Not JSON
  }

  // For Read/Glob/Grep — show line count
  const toolName = tc.name
  if (toolName === 'Read' || toolName === 'Glob' || toolName === 'Grep') {
    const lines = result.Content.split('\n').length
    return `${lines} line${lines !== 1 ? 's' : ''}`
  }

  // Diff output
  if (result.Content.startsWith('diff --git')) {
    const files = result.Content.split('diff --git').length - 1
    return `Diff loaded — ${files} file${files !== 1 ? 's' : ''}`
  }

  // Fallback — truncate
  const text = result.Content
  return text.length > 80 ? text.slice(0, 77) + '...' : text
}

function compactNum(n: number): string {
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
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
