import { useMemo, useState } from 'react'
import Markdown from 'react-markdown'
import type { AgentMessage, AgentRun, ToolCall } from '../types'
import { isActiveRun } from '../lib/runStatus'

export type ViewMode = 'conversation' | 'commands'

interface Props {
  messages: AgentMessage[]
  run: AgentRun
  mode: ViewMode
}

// Transcript renders the full, untruncated run log. Unlike AgentCard's
// activity log, nothing here is summarized or capped — the whole point
// of the detail view is to see exactly what the agent did.
//
// View modes:
//   - conversation: text + tool calls + results (everything except thinking,
//     which isn't persisted today).
//   - commands: tool calls + results only. Prose is hidden so it's easy
//     to scan what the agent actually executed.
export default function Transcript({ messages, run, mode }: Props) {
  const toolResults = useMemo(() => {
    const map = new Map<string, AgentMessage>()
    for (const msg of messages) {
      if (msg.Role === 'tool' && msg.ToolCallID) map.set(msg.ToolCallID, msg)
    }
    return map
  }, [messages])

  const rows: React.ReactNode[] = []

  for (const msg of messages) {
    const time = new Date(msg.CreatedAt).toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })

    if (msg.Role !== 'assistant') continue

    // Skip the raw JSON completion message (the agent's structured
    // exit payload — `{"status": ...}` blob)
    if (msg.Content && msg.Content.trimStart().startsWith('{"status":')) continue

    // Text content
    if (msg.Content && mode !== 'commands') {
      rows.push(
        <Row key={`text-${msg.ID}`} time={time}>
          <div className="prose prose-sm max-w-none text-text-primary leading-relaxed [&_p]:my-2 [&_pre]:bg-black/[0.04] [&_pre]:rounded-lg [&_pre]:p-3 [&_code]:bg-black/[0.04] [&_code]:rounded [&_code]:px-1 [&_code]:py-0.5 [&_code]:text-[12px]">
            <Markdown>{msg.Content}</Markdown>
          </div>
          {(msg.InputTokens || msg.OutputTokens) && (
            <div className="mt-1 text-[10px] text-text-tertiary font-mono">
              {msg.InputTokens ? `↓${msg.InputTokens}` : ''}{' '}
              {msg.OutputTokens ? `↑${msg.OutputTokens}` : ''}
            </div>
          )}
        </Row>,
      )
    }

    if (msg.ToolCalls?.length) {
      for (const tc of msg.ToolCalls) {
        const result = toolResults.get(tc.id)
        rows.push(
          <Row key={`tc-${tc.id}`} time={time}>
            <ToolCallBlock call={tc} result={result} />
          </Row>,
        )
      }
    }
  }

  if (run.ResultSummary && !isActiveRun(run)) {
    const isFailed = run.Status === 'failed' || run.Status === 'cancelled'
    const isUnsolvable = run.Status === 'task_unsolvable'
    rows.push(
      <div
        key="result-summary"
        className="my-4 rounded-2xl backdrop-blur-sm bg-white/60 border border-border-glass p-4"
      >
        <div className="mb-2 text-[11px] font-semibold tracking-wide uppercase">
          <span
            className={
              isFailed ? 'text-dismiss' : isUnsolvable ? 'text-snooze' : 'text-text-primary'
            }
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
        <div className="prose prose-sm max-w-none text-text-secondary leading-relaxed">
          <Markdown>{run.ResultSummary}</Markdown>
        </div>
      </div>,
    )
  }

  if (rows.length === 0) {
    return (
      <div className="text-[13px] text-text-tertiary px-4 py-8 text-center">
        {mode === 'commands' ? 'No commands run yet.' : 'Waiting for agent…'}
      </div>
    )
  }

  return <div className="space-y-1">{rows}</div>
}

function Row({ time, children }: { time: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start gap-3 px-1 py-2 border-b border-border-subtle/40">
      <span className="shrink-0 mt-0.5 text-[10px] text-text-tertiary opacity-60 font-mono w-16">
        {time}
      </span>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  )
}

function ToolCallBlock({ call, result }: { call: ToolCall; result?: AgentMessage }) {
  const [open, setOpen] = useState(false)
  const headline = headlineForToolCall(call)
  const inputJson = useMemo(() => safeJsonStringify(call.input), [call.input])
  const resultText = result?.Content ?? ''
  const isError = !!result?.IsError
  const isLongResult = resultText.length > 400

  return (
    <div className="rounded-lg border border-border-subtle bg-black/[0.015] overflow-hidden">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="w-full flex items-center gap-2 px-3 py-1.5 text-left hover:bg-black/[0.02] transition-colors"
      >
        <span className="text-[10px] font-mono uppercase tracking-wider text-text-tertiary shrink-0">
          {call.name}
        </span>
        <span className="text-[12px] text-text-primary font-medium truncate">{headline}</span>
        <span className="ml-auto text-[10px] text-text-tertiary shrink-0">{open ? '▾' : '▸'}</span>
      </button>

      {open && (
        <div className="px-3 pb-3 pt-1 space-y-2 border-t border-border-subtle/60">
          <div>
            <div className="text-[10px] font-semibold uppercase tracking-wider text-text-tertiary mb-1">
              Input
            </div>
            <pre className="text-[11px] font-mono whitespace-pre-wrap break-words bg-black/[0.04] rounded p-2 text-text-primary max-h-[400px] overflow-auto">
              {inputJson}
            </pre>
          </div>
        </div>
      )}

      {result ? (
        <ResultPane text={resultText} isError={isError} long={isLongResult} />
      ) : (
        <div className="px-3 py-1.5 text-[11px] text-text-tertiary border-t border-border-subtle/60">
          <span className="inline-block w-1.5 h-1.5 rounded-full bg-delegate animate-pulse mr-1.5" />
          Running…
        </div>
      )}
    </div>
  )
}

function ResultPane({ text, isError, long }: { text: string; isError: boolean; long: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const shown = !long || expanded ? text : text.slice(0, 400)
  const truncated = long && !expanded

  return (
    <div
      className={`px-3 py-2 border-t border-border-subtle/60 ${
        isError ? 'bg-dismiss/5' : 'bg-black/[0.01]'
      }`}
    >
      <div className="flex items-center gap-2 mb-1">
        <div
          className={`text-[10px] font-semibold uppercase tracking-wider ${
            isError ? 'text-dismiss' : 'text-text-tertiary'
          }`}
        >
          {isError ? 'Error' : 'Result'}
        </div>
        {long && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-[10px] text-accent hover:underline"
          >
            {expanded ? 'Collapse' : `Show all (${text.length} chars)`}
          </button>
        )}
      </div>
      <pre
        className={`text-[11px] font-mono whitespace-pre-wrap break-words text-text-primary max-h-[500px] overflow-auto ${
          isError ? '' : ''
        }`}
      >
        {shown || (isError ? '(no error body)' : '✓')}
        {truncated && '…'}
      </pre>
    </div>
  )
}

function headlineForToolCall(call: ToolCall): string {
  const input = call.input || {}
  if (call.name === 'Bash') {
    return String(input.command || '(no command)')
  }
  if (call.name === 'Read') return String(input.file_path || '(no path)')
  if (call.name === 'Write') return String(input.file_path || '(no path)')
  if (call.name === 'Edit') return String(input.file_path || '(no path)')
  if (call.name === 'Glob') return String(input.pattern || '(no pattern)')
  if (call.name === 'Grep') return String(input.pattern || '(no pattern)')
  // Fallback: first string-valued input or empty
  for (const v of Object.values(input)) {
    if (typeof v === 'string' && v.length < 200) return v
  }
  return ''
}

function safeJsonStringify(v: unknown): string {
  try {
    // JSON.stringify(undefined) returns undefined, not a string — fall
    // back to String() so callers always get something renderable.
    const s = JSON.stringify(v, null, 2)
    return s ?? String(v)
  } catch {
    return String(v)
  }
}
