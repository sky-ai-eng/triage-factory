import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import { Check, Copy } from 'lucide-react'
import type { AgentMessage, AgentRun, ToolCall } from '../../types'
import { isActiveRun } from '../../lib/runStatus'
import { stripWorktree } from '../../lib/worktree'
import { toast } from '../Toast/toastStore'
import { stationState } from './stationStyle'

interface Props {
  messages: AgentMessage[]
  run: AgentRun
}

// ScreenTranscript — the machine's display. The transcript runs on the screen
// embedded in the station's housing (dark glass in dark mode, warm backlit paper
// in light mode — the --hmi-* vars flip), under faint scanlines and grain.
// Everything is flat; the depth is all light and texture.
//
// Pin-to-bottom is the default, flipped off only by a user-initiated upward
// scroll (any input modality — we read position deltas, not event sources, so
// programmatic auto-scroll never trips the unpin). A ResizeObserver re-snaps as
// late-rendering markdown grows the content.
export default function ScreenTranscript({ messages, run }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  const pinnedRef = useRef(true)
  const lastTopRef = useRef(0)
  // `pinned` drives the jump-to-latest button (render); `pinnedRef` mirrors it
  // so the scroll/observer callbacks read the latest value without
  // re-subscribing. Render reads the state, never the ref.
  const [pinned, setPinnedState] = useState(true)
  const setPinned = (v: boolean) => {
    if (pinnedRef.current === v) return
    pinnedRef.current = v
    setPinnedState(v)
  }

  useEffect(() => {
    if (!pinnedRef.current) return
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [messages, run])

  useEffect(() => {
    const el = scrollRef.current
    const content = contentRef.current
    if (!el || !content) return
    const ro = new ResizeObserver(() => {
      if (pinnedRef.current) el.scrollTop = el.scrollHeight
    })
    ro.observe(content)
    return () => ro.disconnect()
  }, [])

  const onScroll = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    const top = el.scrollTop
    const prev = lastTopRef.current
    lastTopRef.current = top
    const atBottom = el.scrollHeight - (top + el.clientHeight) < 36
    if (atBottom) {
      if (!pinnedRef.current) setPinned(true)
      return
    }
    if (top < prev) setPinned(false)
  }, [])

  const rows = useMemo(() => buildRows(messages, run), [messages, run])

  return (
    <div className="relative min-h-0 flex-1 overflow-hidden rounded-[5px]">
      {/* The screen field — dark glass in dark mode, warm backlit paper in light
          mode (the --hmi-* vars flip). A soft inset recess sinks it into the
          housing; it's lit only by its own content. */}
      <div
        className="absolute inset-0"
        style={{
          background: `radial-gradient(120% 90% at 50% 0%, var(--hmi-screen-lift), var(--hmi-screen) 70%)`,
          boxShadow: 'inset 0 0 70px var(--hmi-recess)',
        }}
      />

      {/* Scanlines — fixed horizontal rule texture, the CRT/HMI tell. */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          backgroundImage:
            'repeating-linear-gradient(0deg, var(--hmi-scanline) 0px, var(--hmi-scanline) 1px, transparent 1px, transparent 3px)',
        }}
      />
      {/* Grain flicker — barely there, just enough to keep the glass alive. */}
      <div
        aria-hidden
        className="hmi-anim pointer-events-none absolute inset-0 mix-blend-overlay"
        style={{
          backgroundImage:
            'radial-gradient(var(--hmi-grain) 0.5px, transparent 0.6px), radial-gradient(var(--hmi-grain) 0.5px, transparent 0.6px)',
          backgroundSize: '3px 3px, 5px 5px',
          backgroundPosition: '0 0, 1px 2px',
          animation: 'hmi-flicker 3.2s steps(2) infinite',
        }}
      />

      {/* Top dissolve — older lines fade under the bezel as the feed grows. */}
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 z-10 h-12"
        style={{ background: 'linear-gradient(to bottom, var(--hmi-screen), transparent)' }}
      />

      {/* The readout itself — content scales with the screen width, holding just
          a little side padding (a touch more once there's room). No max width. */}
      <div
        ref={scrollRef}
        onScroll={onScroll}
        tabIndex={0}
        role="log"
        aria-label="Agent run transcript"
        className="absolute inset-0 overflow-y-auto px-5 py-5 focus:outline-none md:px-8"
      >
        <div ref={contentRef}>
          {rows.length === 0 ? <EmptyReadout active={isActiveRun(run)} /> : rows}
        </div>
      </div>

      {!pinned && (
        <button
          type="button"
          onClick={() => {
            const el = scrollRef.current
            if (el) el.scrollTop = el.scrollHeight
            setPinned(true)
          }}
          className="absolute bottom-4 left-1/2 z-20 inline-flex -translate-x-1/2 items-center gap-1.5 rounded-full px-3 py-1.5 font-mono text-[10px] font-semibold uppercase tracking-[0.14em] backdrop-blur-sm transition-transform hover:scale-[1.02]"
          style={{
            color: 'var(--hmi-ink-dim)',
            background: 'color-mix(in srgb, var(--hmi-screen-lift) 80%, transparent)',
            boxShadow: 'inset 0 0 0 1px var(--hmi-line)',
          }}
        >
          <svg width="11" height="11" viewBox="0 0 16 16" fill="none" aria-hidden>
            <path
              d="M8 3v9m0 0l-4-4m4 4l4-4"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
          Latest
        </button>
      )}
    </div>
  )
}

// buildRows flattens the transcript into screen rows: assistant prose and tool
// calls (each paired with its result), the JSON completion blob skipped.
function buildRows(messages: AgentMessage[], run: AgentRun): React.ReactNode[] {
  const toolResults = new Map<string, AgentMessage>()
  for (const m of messages) {
    if (m.Role === 'tool' && m.ToolCallID) toolResults.set(m.ToolCallID, m)
  }

  const rows: React.ReactNode[] = []
  for (const msg of messages) {
    if (msg.Role !== 'assistant') continue
    if (msg.Content && msg.Content.trimStart().startsWith('{"status":')) continue
    const time = clockTime(msg.CreatedAt)

    // Reasoning gets its own quiet row — dim italic under a THINKING tag —
    // so it reads as the agent's interior voice, distinct from prose output.
    if (msg.Subtype === 'thinking') {
      if (msg.Content) {
        rows.push(
          <ScreenRow key={`th-${msg.ID}`} time={time}>
            <ThinkingLine text={msg.Content} />
          </ScreenRow>,
        )
      }
      continue
    }

    if (msg.Content) {
      rows.push(
        <ScreenRow key={`t-${msg.ID}`} time={time}>
          <div
            className="max-w-none text-[13px] leading-relaxed [&_a]:text-[color:var(--hmi-cyan-bright)] [&_a]:underline [&_code]:rounded-[3px] [&_code]:bg-[color:var(--hmi-code-bg)] [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-[11.5px] [&_h1]:mb-1 [&_h1]:mt-2 [&_h1]:text-[15px] [&_h1]:font-semibold [&_h2]:mb-1 [&_h2]:mt-2 [&_h2]:text-[14px] [&_h2]:font-semibold [&_li]:my-0.5 [&_ol]:my-2 [&_ol]:list-decimal [&_ol]:pl-5 [&_p]:my-1.5 [&_pre]:my-2 [&_pre]:overflow-auto [&_pre]:rounded-[4px] [&_pre]:bg-[color:var(--hmi-code-bg)] [&_pre]:p-3 [&_pre]:text-[11.5px] [&_strong]:font-semibold [&_strong]:text-[color:var(--hmi-strong)] [&_ul]:my-2 [&_ul]:list-disc [&_ul]:pl-5"
            style={{ color: 'var(--hmi-ink)' }}
          >
            <Markdown>{msg.Content}</Markdown>
          </div>
        </ScreenRow>,
      )
    }

    if (msg.ToolCalls?.length) {
      for (const tc of msg.ToolCalls) {
        rows.push(
          <ScreenRow key={`c-${tc.id}`} time={time}>
            <ToolLine call={tc} result={toolResults.get(tc.id)} worktree={run.WorktreePath} />
          </ScreenRow>,
        )
      }
    }
  }

  if (run.ResultSummary && !isActiveRun(run)) {
    rows.push(<Verdict key="verdict" run={run} />)
  }
  return rows
}

// ScreenRow — a readout line: a dim mono timestamp in the left gutter, vertically
// centered against the content (so it sits mid-block on multi-line entries),
// content to the right, separated by the faintest rule.
function ScreenRow({ time, children }: { time: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-3 border-b border-[color:var(--hmi-line)] py-2.5">
      <span
        className="w-14 shrink-0 self-center font-mono text-[10px] tabular-nums tracking-tight"
        style={{ color: 'var(--hmi-ink-dim)' }}
      >
        {time}
      </span>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  )
}

// ThinkingLine — the agent's reasoning, kept quiet on the screen: dim italic
// ink under a small THINKING tag, clamped to a few lines until toggled open.
function ThinkingLine({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  const long = text.length > 320
  return (
    <div>
      <div className="mb-0.5 flex items-center gap-2">
        <span
          className="font-mono text-[9px] font-semibold uppercase tracking-[0.16em]"
          style={{ color: 'var(--hmi-ink-dim)' }}
        >
          thinking
        </span>
        {long && (
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            className="font-mono text-[9px] uppercase tracking-wider hover:underline"
            style={{ color: 'var(--hmi-cyan)' }}
          >
            {open ? 'less' : 'more'}
          </button>
        )}
      </div>
      <div
        className={`whitespace-pre-wrap text-[12px] italic leading-relaxed ${open ? '' : 'line-clamp-3'}`}
        style={{ color: 'var(--hmi-ink-dim)' }}
      >
        {text}
      </div>
    </div>
  )
}

// ToolLine — a tool call as a machine log entry: cyan tool tag + headline, the
// input/result expandable beneath.
function ToolLine({
  call,
  result,
  worktree,
}: {
  call: ToolCall
  result?: AgentMessage
  worktree?: string
}) {
  const [open, setOpen] = useState(false)
  // Collapse the agent's absolute worktree paths to worktree-relative ones
  // everywhere they show up — the headline, the input args, and the output.
  const headline = stripWorktree(headlineForToolCall(call), worktree)
  const inputJson = useMemo(
    () => stripWorktree(safeJsonStringify(call.input), worktree),
    [call.input, worktree],
  )
  const resultText = stripWorktree(result?.Content ?? '', worktree)
  const isError = !!result?.IsError
  const long = resultText.length > 400

  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="group/tool flex w-full items-center gap-2 text-left"
      >
        <span
          className="shrink-0 font-mono text-[10px] font-semibold uppercase tracking-[0.12em]"
          style={{ color: 'var(--hmi-cyan)' }}
        >
          {call.name}
        </span>
        <span
          className="min-w-0 flex-1 truncate font-mono text-[12px]"
          style={{ color: 'var(--hmi-ink)' }}
        >
          {headline}
        </span>
        <span
          className="shrink-0 font-mono text-[10px] opacity-50 transition-opacity group-hover/tool:opacity-90"
          style={{ color: 'var(--hmi-ink-dim)' }}
        >
          {open ? '−' : '+'}
        </span>
      </button>

      {open && (
        <div className="mt-2 space-y-2">
          <Pane label="IN">{inputJson}</Pane>
          {result && (
            <Pane label={isError ? 'ERR' : 'OUT'} error={isError} truncatable={long}>
              {resultText || (isError ? '(no error body)' : 'ok')}
            </Pane>
          )}
        </div>
      )}
    </div>
  )
}

// Pane — a segmented IN / OUT block on the screen. Shows the (optionally
// truncated) content with a faded copy affordance that flips to a check for a
// beat; the copy always grabs the FULL content, never the truncated view.
function Pane({
  label,
  children,
  error,
  truncatable,
}: {
  label: string
  children: string
  error?: boolean
  truncatable?: boolean
}) {
  const [expanded, setExpanded] = useState(false)
  const [copied, setCopied] = useState(false)
  const copiedTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(() => () => clearTimeout(copiedTimer.current ?? undefined), [])
  const text = String(children)
  const shown = !truncatable || expanded ? text : text.slice(0, 400)

  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      clearTimeout(copiedTimer.current ?? undefined)
      setCopied(true)
      copiedTimer.current = setTimeout(() => setCopied(false), 1000)
    } catch (err) {
      toast.error(`Failed to copy: ${(err as Error).message}`)
    }
  }

  return (
    <div
      className="rounded-[4px] border-l-2 py-1.5 pl-3 pr-2"
      style={{
        borderColor: error
          ? 'var(--color-dismiss)'
          : 'color-mix(in srgb, var(--hmi-cyan) 40%, transparent)',
        background: 'var(--hmi-code-bg)',
      }}
    >
      <div className="mb-1 flex items-center gap-2">
        <span
          className="font-mono text-[9px] font-semibold uppercase tracking-[0.16em]"
          style={{ color: error ? 'var(--color-dismiss)' : 'var(--hmi-ink-dim)' }}
        >
          {label}
        </span>
        {truncatable && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="font-mono text-[9px] uppercase tracking-wider hover:underline"
            style={{ color: 'var(--hmi-cyan)' }}
          >
            {expanded ? 'less' : `${text.length} ch`}
          </button>
        )}
        <button
          type="button"
          onClick={onCopy}
          aria-label={`Copy ${label}`}
          title="Copy full content"
          className="ml-auto inline-flex items-center opacity-40 transition-opacity hover:opacity-100"
          style={{ color: copied ? 'var(--hmi-cyan)' : 'var(--hmi-ink-dim)' }}
        >
          {copied ? <Check size={11} /> : <Copy size={11} />}
        </button>
      </div>
      <pre
        className="max-h-[420px] overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed"
        style={{ color: error ? 'var(--color-dismiss)' : 'var(--hmi-ink)' }}
      >
        {shown}
        {truncatable && !expanded ? '…' : ''}
      </pre>
    </div>
  )
}

// Verdict — the settled run's outcome, stamped on the screen in the state tone.
function Verdict({ run }: { run: AgentRun }) {
  const st = stationState(run)
  return (
    <div className="mb-2 mt-5">
      <div className="mb-2 flex items-center gap-2">
        <span
          className="h-px w-6"
          style={{ background: st.light, boxShadow: `0 0 8px ${st.light}` }}
        />
        <span
          className="font-mono text-[10px] font-semibold uppercase tracking-[0.18em]"
          style={{ color: st.light }}
        >
          {st.label}
        </span>
      </div>
      <div
        className="max-w-none text-[12.5px] leading-relaxed [&_code]:rounded-[3px] [&_code]:bg-[color:var(--hmi-code-bg)] [&_code]:px-1 [&_code]:font-mono [&_code]:text-[11px] [&_li]:my-0.5 [&_p]:my-1.5 [&_ul]:my-2 [&_ul]:list-disc [&_ul]:pl-5"
        style={{ color: 'var(--hmi-ink)' }}
      >
        <Markdown>{run.ResultSummary}</Markdown>
      </div>
    </div>
  )
}

function EmptyReadout({ active }: { active: boolean }) {
  return (
    <div className="flex h-40 items-center justify-center">
      <span
        className="inline-flex items-center gap-2 font-mono text-[11px]"
        style={{ color: 'var(--hmi-ink-dim)' }}
      >
        {active && (
          <span
            className="hmi-anim h-1.5 w-1.5 rounded-full"
            style={{
              background: 'var(--hmi-cyan-bright)',
              animation: 'hmi-core 1.4s ease-in-out infinite',
            }}
          />
        )}
        {active ? 'awaiting agent…' : 'no output'}
      </span>
    </div>
  )
}

// ── helpers (ported from the old Transcript) ──────────────────────────────

function clockTime(iso: string): string {
  return new Date(iso).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
}

function headlineForToolCall(call: ToolCall): string {
  const input = call.input || {}
  // Bash: prefer the agent-authored description — the human-readable intent.
  // The raw command stays one click away in the expanded IN pane.
  if (call.name === 'Bash') return String(input.description || input.command || '(no command)')
  if (call.name === 'Read' || call.name === 'Write' || call.name === 'Edit')
    return String(input.file_path || '(no path)')
  if (call.name === 'Glob') return String(input.pattern || '(no pattern)')
  if (call.name === 'Grep') return String(input.pattern || '(no pattern)')
  for (const v of Object.values(input)) {
    if (typeof v === 'string' && v.length < 200) return v
  }
  return ''
}

function safeJsonStringify(v: unknown): string {
  try {
    const s = JSON.stringify(v, null, 2)
    return s ?? String(v)
  } catch {
    return String(v)
  }
}
