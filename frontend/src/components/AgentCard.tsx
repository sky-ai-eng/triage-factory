import { useEffect, useRef, useState } from 'react'
import type { AgentMessage, AgentRun, Task, ToolCall } from '../types'

interface Props {
  task: Task
  run: AgentRun
  messages: AgentMessage[]
  onRequeue?: () => void
  onReview?: () => void
}

export default function AgentCard({ task, run, messages, onRequeue, onReview }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const [now, setNow] = useState(() => Date.now())

  const isActive = [
    'cloning',
    'fetching',
    'worktree_created',
    'agent_starting',
    'running',
  ].includes(run.Status)

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
    !isActive && run.DurationMs ? formatDurationMs(run.DurationMs) : formatElapsed(run.StartedAt, now)
  const isFailed = run.Status === 'failed'
  const isCancelled = run.Status === 'cancelled'
  const isPendingApproval = run.Status === 'pending_approval'

  const statusColor =
    isFailed || isCancelled
      ? 'text-dismiss'
      : isPendingApproval
        ? 'text-snooze'
        : isActive
          ? 'text-delegate'
          : 'text-claim'

  const statusIcon = isFailed
    ? '✗'
    : isCancelled
      ? '◼'
      : isPendingApproval
        ? '◉'
        : isActive
          ? '●'
          : '✓'
  const statusLabel = formatStatus(run.Status)

  const stats = computeStats(messages, run)

  return (
    <div className="bg-surface-raised backdrop-blur-xl border border-border-glass rounded-2xl overflow-hidden shadow-sm shadow-black/[0.03]">
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
            <span className="text-[11px] text-text-tertiary">{elapsed}</span>
            {isActive && (
              <button
                onClick={() => fetch(`/api/agent/runs/${run.ID}/cancel`, { method: 'POST' })}
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
        <div className="flex items-center gap-2 text-[11px] text-text-tertiary">
          <span
            className={`font-medium uppercase tracking-wider px-1.5 py-0.5 rounded ${
              task.source === 'github' ? 'bg-black/[0.04]' : 'bg-blue-500/10 text-blue-600'
            }`}
          >
            {task.source === 'github' ? 'GH' : 'Jira'}
          </span>
          {task.repo && <span>{task.repo}</span>}
          {task.source === 'github' && <span>#{task.source_id.split('#').pop()}</span>}
        </div>
      </div>

      {/* Activity log */}
      <div
        ref={scrollRef}
        className="mx-3 mb-3 rounded-xl bg-black/[0.02] border border-border-subtle max-h-[200px] overflow-y-auto"
      >
        {messages.length === 0 && isActive && (
          <div className="px-4 py-3 text-[12px] text-text-tertiary">Waiting for agent...</div>
        )}
        {renderActivityLog(messages, isActive, run)}
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
          {(isFailed || isCancelled) && onRequeue && (
            <button
              onClick={onRequeue}
              className="text-[12px] text-text-tertiary hover:text-text-primary font-medium transition-colors"
            >
              Return to queue
            </button>
          )}
          {isPendingApproval && onReview && (
            <button
              onClick={onReview}
              className="text-[12px] font-semibold text-snooze bg-snooze/10 hover:bg-snooze/20 px-3 py-1 rounded-lg transition-colors"
            >
              Review
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
    if (msg.Role !== 'assistant') continue

    const time = new Date(msg.CreatedAt).toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })

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
    elements.push(
      <div
        key="result-summary"
        className="mx-2 my-2 rounded-xl backdrop-blur-sm bg-white/50 border border-border-glass p-3.5"
      >
        <div className="flex items-center justify-between mb-2">
          <span
            className={`text-[11px] font-semibold tracking-wide ${isFailed ? 'text-dismiss' : 'text-text-primary'}`}
          >
            {run.Status === 'cancelled' ? '◼ Cancelled' : isFailed ? '✗ Failed' : '✓ Done'}
          </span>
          {run.ResultLink && (
            <a
              href={run.ResultLink}
              target="_blank"
              rel="noopener noreferrer"
              className="text-[11px] text-accent hover:text-accent/70 font-medium transition-colors"
            >
              View →
            </a>
          )}
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
    if (cmd.includes('todotriage exec gh pr view')) return 'Fetching PR details'
    if (cmd.includes('todotriage exec gh pr diff') && cmd.includes('--file'))
      return `Reading diff: ${extractFlag(cmd, '--file')}`
    if (cmd.includes('todotriage exec gh pr diff')) return 'Reading full diff'
    if (cmd.includes('todotriage exec gh pr files')) return 'Listing changed files'
    if (cmd.includes('todotriage exec gh pr review-view')) return 'Expanding previous review'
    if (cmd.includes('todotriage exec gh pr start-review')) return 'Starting review'
    if (cmd.includes('todotriage exec gh pr add-review-comment')) {
      const file = extractFlag(cmd, '--file')
      return file ? `Adding comment on ${file}` : 'Adding review comment'
    }
    if (cmd.includes('todotriage exec gh pr submit-review')) {
      const event = extractFlag(cmd, '--event')
      return `Submitting review (${event || 'comment'})`
    }
    if (cmd.includes('todotriage exec gh pr comment-list-pending'))
      return 'Reviewing pending comments'
    if (cmd.includes('todotriage exec gh pr add-comment')) return 'Adding comment'
    if (cmd.includes('todotriage exec'))
      return `Running: ${cmd.split('todotriage exec ')[1]?.slice(0, 60)}`
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

function formatStatus(status: string): string {
  const map: Record<string, string> = {
    cloning: 'Pulling repo',
    fetching: 'Fetching PR details',
    worktree_created: 'Creating worktree',
    agent_starting: 'Starting Claude Code',
    running: 'Running',
    completed: 'Completed',
    pending_approval: 'Pending Approval',
    cancelled: 'Cancelled',
    failed: 'Failed',
  }
  return map[status] || status
}

function formatDurationMs(ms: number): string {
  const seconds = Math.floor(ms / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  if (minutes < 60) return `${minutes}m ${secs}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
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

function formatElapsed(dateStr: string, now: number = Date.now()): string {
  const diff = now - new Date(dateStr).getTime()
  const seconds = Math.floor(diff / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  if (minutes < 60) return `${minutes}m ${secs}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

function compactNum(n: number): string {
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
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
