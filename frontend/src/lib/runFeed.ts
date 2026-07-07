import type { AgentMessage } from '../types'

// runFeed — the bounded per-run projection the board's AgentCards render from.
//
// The board used to hold every run's FULL message array in top-level state and
// hand it to each card, which then derived a token/comment tally and the last
// five ticker lines on every render. That meant unbounded memory growth per
// live run AND a whole-board re-render doing O(all messages) work for every
// streamed agent_message. The card only ever displays aggregates + the tail,
// so this module keeps exactly that: running stats plus the last few feed
// lines, updated incrementally as messages arrive.
//
// The full transcript still lives where it's actually shown — the run detail
// page (useRunDetail / ScreenTranscript).

export interface FeedLine {
  id: string
  time: string
  text: string
}

export interface RunCardFeed {
  /** Count of review/PR comments the agent has posted (same heuristic the old
   *  card-level computeStats used: first tool call's command mentions
   *  add-review-comment / add-comment). */
  comments: number
  /** Total tokens (input + output) across every message seen. */
  tokens: number
  /** The most recent ticker lines, oldest-first, capped at FEED_LINE_CAP. */
  lines: FeedLine[]
}

export const EMPTY_FEED: RunCardFeed = { comments: 0, tokens: 0, lines: [] }

// The card's LiveFeed shows the last 5 lines; keep a small buffer beyond that
// so the cap never changes what's displayed.
const FEED_LINE_CAP = 8

/** Build a feed from a full message array (the aggregated board fetch). */
export function feedFromMessages(messages: AgentMessage[]): RunCardFeed {
  let feed = EMPTY_FEED
  for (const msg of messages) feed = appendToFeed(feed, msg)
  return feed
}

/**
 * Fold one streamed message into a feed. Returns the SAME reference when the
 * message changes nothing the card displays — no ticker lines, no token
 * delta, no comment delta (tool-result rows are the common case: roughly half
 * a live transcript). Callers rely on that identity to skip the state write
 * entirely, so a display-no-op message doesn't re-render the board.
 */
export function appendToFeed(prev: RunCardFeed | undefined, msg: AgentMessage): RunCardFeed {
  const base = prev ?? EMPTY_FEED
  const lines = linesForMessage(msg)
  const comments = commentsForMessage(msg)
  const tokens = (msg.OutputTokens ?? 0) + (msg.InputTokens ?? 0)
  if (lines.length === 0 && comments === 0 && tokens === 0) return base
  return {
    comments: base.comments + comments,
    tokens: base.tokens + tokens,
    lines: lines.length === 0 ? base.lines : [...base.lines, ...lines].slice(-FEED_LINE_CAP),
  }
}

function commentsForMessage(msg: AgentMessage): number {
  if (msg.Role === 'assistant' && msg.Subtype === 'tool_use' && msg.ToolCalls?.length) {
    const cmd = String(msg.ToolCalls[0].input?.command || '')
    if (cmd.includes('add-review-comment') || cmd.includes('add-comment')) return 1
  }
  return 0
}

// linesForMessage flattens one message into compact one-liners — the agent's
// actions (tool calls) and its prose turns — for the card's live ticker. The
// full, nested transcript lives in the expanded run view.
function linesForMessage(msg: AgentMessage): FeedLine[] {
  const out: FeedLine[] = []
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
    return out
  }
  if (msg.Role !== 'assistant') return out
  // Skip the raw JSON completion message (the agent's structured output).
  if (msg.Content && msg.Content.trimStart().startsWith('{"status":')) return out
  // Reasoning stays off the ticker — it's a verbose stream; the expanded
  // run view renders it under its own THINKING rows.
  if (msg.Subtype === 'thinking') return out
  if (msg.Content) out.push({ id: `txt-${msg.ID}`, time, text: clip(msg.Content, 70) })
  if (msg.ToolCalls?.length) {
    for (const tc of msg.ToolCalls) {
      out.push({ id: `tc-${tc.id}`, time, text: formatToolCall(tc.name, tc.input) })
    }
  }
  return out
}

function clip(s: string, n: number): string {
  const t = s.replace(/\s+/g, ' ').trim()
  return t.length > n ? t.slice(0, n - 1) + '…' : t
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
