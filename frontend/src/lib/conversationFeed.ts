import type { Message } from '../types'
import { isSystemNotice } from './messageVoice'
import { bashHeadline, firstLine, isBashTool } from './toolHeadline'

// conversationFeed — the bounded per-conversation projection the board's AgentCards render from.
//
// The board used to hold every conversation's FULL message array in top-level state and
// hand it to each card, which then derived a token tally and the last five
// ticker lines on every render. That meant unbounded memory growth per
// live conversation AND a whole-board re-render doing O(all messages) work for every
// streamed `message` event. The card only ever displays aggregates + the tail,
// so this module keeps exactly that: running stats plus the last few feed
// lines, updated incrementally as messages arrive.
//
// The full transcript still lives where it's actually shown — the conversation detail
// page (useConversationDetail / ScreenTranscript).

export interface FeedLine {
  id: string
  time: string
  text: string
}

export interface ConversationCardFeed {
  /** Total tokens (input + output) across every message seen. */
  tokens: number
  /** The most recent ticker lines, oldest-first, capped at FEED_LINE_CAP. */
  lines: FeedLine[]
}

export const EMPTY_FEED: ConversationCardFeed = { tokens: 0, lines: [] }

// The card's LiveFeed shows the last 5 lines; keep a small buffer beyond that
// so the cap never changes what's displayed.
const FEED_LINE_CAP = 8

/** Build a feed from a full message array (the aggregated board fetch). */
export function feedFromMessages(messages: Message[]): ConversationCardFeed {
  let feed = EMPTY_FEED
  for (const msg of messages) feed = appendToFeed(feed, msg)
  return feed
}

/**
 * Fold one streamed message into a feed. Returns the SAME reference when the
 * message changes nothing the card displays — no ticker lines, no token delta
 * (tool-result rows are the common case: roughly half a live transcript).
 * Callers rely on that identity to skip the state write entirely, so a
 * display-no-op message doesn't re-render the board.
 */
export function appendToFeed(
  prev: ConversationCardFeed | undefined,
  msg: Message,
): ConversationCardFeed {
  const base = prev ?? EMPTY_FEED
  const lines = linesForMessage(msg)
  const tokens = (msg.output_tokens ?? 0) + (msg.input_tokens ?? 0)
  if (lines.length === 0 && tokens === 0) return base
  return {
    tokens: base.tokens + tokens,
    lines: lines.length === 0 ? base.lines : [...base.lines, ...lines].slice(-FEED_LINE_CAP),
  }
}

// linesForMessage flattens one message into compact one-liners — the agent's
// actions (tool calls) and its prose turns — for the card's live ticker. The
// full, nested transcript lives in the expanded conversation view.
function linesForMessage(msg: Message): FeedLine[] {
  const out: FeedLine[] = []
  const time = new Date(msg.created_at).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
  // Operator steers show on the ticker too — the card should reflect that
  // someone redirected the conversation. A system-authored user row (a stop note, an
  // executor-changed notice) rides the same role and is NOT that: it shows
  // without the attribution, because prefixing the machine's own words with
  // "you:" tells the reader they said something they never said.
  if (msg.role === 'user' && msg.content) {
    const text = isSystemNotice(msg) ? clip(msg.content, 70) : `you: ${clip(msg.content, 64)}`
    out.push({ id: `u-${msg.id}`, time, text })
    return out
  }
  if (msg.role !== 'assistant') return out
  // Skip the raw JSON completion message (the agent's structured output).
  if (msg.content && msg.content.trimStart().startsWith('{"status":')) return out
  // Reasoning stays off the ticker — it's a verbose stream; the expanded
  // conversation view renders it under its own THINKING rows.
  if (msg.subtype === 'thinking') return out
  if (msg.content) out.push({ id: `txt-${msg.id}`, time, text: clip(msg.content, 70) })
  if (msg.tool_calls?.length) {
    for (const tc of msg.tool_calls) {
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
  // The ticker is a live strip of what is happening now, so a bash line reads
  // in the present tense whether or not its result has landed yet — there is
  // nothing here to pair a call with its result, and past tense on a call
  // still running would be a claim this projection cannot make.
  if (isBashTool(name)) return bashHeadline(input, 'running', curatedCommandLine)
  if (name === 'Read') return `Reading ${basename(String(input.file_path || ''))}`
  if (name === 'Glob') return `Searching for ${String(input.pattern || 'files')}`
  if (name === 'Grep') return `Searching for "${String(input.pattern || '').slice(0, 40)}"`
  return `${name}`
}

// curatedCommandLine is the ticker's last step: the shell command itself, in
// the narrow width a card gives it. TF's own exec verbs get named readings —
// a card has room for "Reading full diff" and not for the argv that means it,
// and these are commands we author, so the reading is exact rather than a
// guess at someone else's shell.
function curatedCommandLine(cmd: string): string {
  if (cmd.includes('triagefactory exec')) {
    if (cmd.includes('triagefactory exec gh pr view')) return 'Fetching PR details'
    if (cmd.includes('triagefactory exec gh pr diff')) {
      // The flag's value is what the reading needs, so its value is what the
      // branch tests: a `--file` with nothing after it reads as the whole
      // diff rather than as a sentence that stops at its colon.
      const file = extractFlag(cmd, '--file')
      return file ? `Reading diff: ${file}` : 'Reading full diff'
    }
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
    // An unrecognized exec verb reads as its own argv — but only when there is
    // argv to read. The applet name can appear with nothing after it, on its
    // own line, or quoted inside some other command, and each of those splits
    // to nothing; naming the command itself is the honest answer there.
    const argv = cmd.split('triagefactory exec ')[1]?.trim()
    if (argv) return `Running: ${argv.slice(0, 60)}`
  }
  return firstLine(cmd) || 'Running command'
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
