import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft } from 'lucide-react'
import type { AgentRun } from '../types'
import { useRunDetail } from '../hooks/useRunDetail'
import { useOrgHref } from '../hooks/useOrgHref'
import { formatDurationMs, formatElapsed, isActiveRun, statusDisplay } from '../lib/runStatus'
import Transcript, { type ViewMode } from '../components/Transcript'
import ChainStepsRail from '../components/ChainStepsRail'
import SourceBadge from '../components/SourceBadge'
import TakeoverModal, { type TakeoverInfo } from '../components/TakeoverModal'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'

const VIEW_MODE_KEY = 'tf:runDetail:viewMode'

function loadInitialMode(): ViewMode {
  try {
    const v = localStorage.getItem(VIEW_MODE_KEY)
    if (v === 'conversation' || v === 'commands') return v
  } catch {
    // localStorage unavailable — fall through to default.
  }
  return 'conversation'
}

// RunDetail is the full-screen view of one agent run. Lives at
// /board/runs/:runID and is the deep-linkable counterpart to the Board's
// AgentCard — same data, no truncation, plus per-message tokens, full
// tool inputs/outputs, and view-mode toggles for scanning either the
// whole transcript or just the commands.
export default function RunDetail() {
  const { runID } = useParams<{ runID: string }>()
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const { run, task, messages, loading, notFound, error } = useRunDetail(runID)
  const [chainSteps, setChainSteps] = useState<AgentRun[] | null>(null)
  const [mode, setMode] = useState<ViewMode>(() => loadInitialMode())
  const [now, setNow] = useState(() => Date.now())
  const [takeoverInfo, setTakeoverInfo] = useState<TakeoverInfo | null>(null)
  const [takeoverPending, setTakeoverPending] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  // Pin-to-bottom is the default. Flipped off only by a user-initiated
  // upward scroll (wheel/touch/keyboard). Programmatic scrolls don't
  // count — otherwise the auto-scroll itself can race with markdown
  // layout and unset the pin.
  const pinnedRef = useRef(true)
  const [, forcePinRender] = useState(0)
  const setPinned = (v: boolean) => {
    if (pinnedRef.current === v) return
    pinnedRef.current = v
    forcePinRender((n) => n + 1)
  }

  useEffect(() => {
    try {
      localStorage.setItem(VIEW_MODE_KEY, mode)
    } catch {
      // ignore
    }
  }, [mode])

  // Ticker for live elapsed display.
  useEffect(() => {
    if (!run || !isActiveRun(run)) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [run])

  // Snap to bottom whenever new messages arrive or the run row
  // refreshes (status flips can grow the result-summary card). Markdown
  // and code blocks may render asynchronously and grow the content
  // *after* this effect runs — the ResizeObserver below catches that.
  useEffect(() => {
    if (!pinnedRef.current) return
    const el = scrollRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
  }, [messages, run])

  // Re-snap on any content size change while pinned. This is what
  // actually keeps the view glued to the bottom — late-rendering
  // markdown, expanding tool result panes, images loading, etc.
  useEffect(() => {
    const el = scrollRef.current
    const content = contentRef.current
    if (!el || !content) return
    const ro = new ResizeObserver(() => {
      if (pinnedRef.current) {
        el.scrollTop = el.scrollHeight
      }
    })
    ro.observe(content)
    return () => ro.disconnect()
  }, [])

  // Scroll handler that covers every input modality (wheel, touch,
  // keyboard, scrollbar drag) by reading position deltas instead of
  // event sources. Any decrease in scrollTop means upward motion —
  // programmatic auto-scrolls only push scrollTop *up* toward
  // scrollHeight, so they never trip the unpin branch.
  const lastScrollTopRef = useRef(0)
  const onScroll = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    const top = el.scrollTop
    const prev = lastScrollTopRef.current
    lastScrollTopRef.current = top
    const atBottom = el.scrollHeight - (top + el.clientHeight) < 32
    if (atBottom) {
      if (!pinnedRef.current) setPinned(true)
      return
    }
    if (top < prev) setPinned(false)
  }, [])

  // Load chain steps if this run is part of a chain. Pads missing
  // steps with synthetic "pending" placeholders so the rail can render
  // the full length of the chain before later steps have spawned.
  useEffect(() => {
    if (!run?.blueprint_run_id) {
      setChainSteps(null)
      return
    }
    let cancelled = false
    fetch(`/api/blueprint-runs/${run.blueprint_run_id}`)
      .then((r) => (r.ok ? r.json() : null))
      .then(
        (
          data: {
            steps?: Array<{ step: { step_index: number }; run?: AgentRun | null }>
          } | null,
        ) => {
          if (cancelled || !data?.steps) return
          const padded: AgentRun[] = data.steps.map((s, i) => {
            if (s.run) return s.run
            return {
              ID: `__pending-${run.blueprint_run_id}-${i}`,
              TaskID: run.TaskID,
              Status: 'pending',
              Model: '',
              StartedAt: '',
              ResultSummary: '',
              blueprint_run_id: run.blueprint_run_id,
              blueprint_step_index: i,
            } as unknown as AgentRun
          })
          setChainSteps(padded)
        },
      )
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [run?.blueprint_run_id, run?.TaskID])

  // Keyboard shortcuts: Esc → back, 1/2 → modes, t → take over.
  // handleTakeover is declared below; capture via ref so the listener
  // doesn't need to re-bind every time `run` updates.
  const handleTakeoverRef = useRef<() => void>(() => {})
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.target instanceof HTMLElement) {
        const tag = e.target.tagName
        if (tag === 'INPUT' || tag === 'TEXTAREA' || e.target.isContentEditable) return
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        navigate(orgHref('/board'))
      } else if (e.key === '1') {
        setMode('conversation')
      } else if (e.key === '2') {
        setMode('commands')
      } else if (e.key === 't' || e.key === 'T') {
        const canTakeOverNow = run?.Status === 'running' && !!run.SessionID && !takeoverPending
        if (canTakeOverNow) {
          e.preventDefault()
          handleTakeoverRef.current()
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate, orgHref, run?.Status, run?.SessionID, takeoverPending])

  const handleTakeover = useCallback(async () => {
    if (!run) return
    setTakeoverPending(true)
    try {
      const res = await fetch(`/api/agent/runs/${run.ID}/takeover`, { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to take over run'))
        return
      }
      setTakeoverInfo((await res.json()) as TakeoverInfo)
    } catch (err) {
      toast.error(`Failed to take over run: ${(err as Error).message}`)
    } finally {
      setTakeoverPending(false)
    }
  }, [run])
  handleTakeoverRef.current = handleTakeover

  const handleCancel = useCallback(async () => {
    if (!run) return
    try {
      const res = await fetch(`/api/agent/runs/${run.ID}/cancel`, { method: 'POST' })
      if (!res.ok) toast.error(await readError(res, 'Failed to cancel run'))
    } catch (err) {
      toast.error(`Failed to cancel run: ${(err as Error).message}`)
    }
  }, [run])

  const handleRequeue = useCallback(async () => {
    if (!run?.TaskID) return
    try {
      const res = await fetch(`/api/tasks/${run.TaskID}/requeue`, { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to return to queue'))
        return
      }
      navigate(orgHref('/board'))
    } catch (err) {
      toast.error(`Failed to return to queue: ${(err as Error).message}`)
    }
  }, [navigate, orgHref, run?.TaskID])

  const elapsed = useMemo(() => {
    if (!run) return ''
    if (!isActiveRun(run) && run.DurationMs != null) return formatDurationMs(run.DurationMs)
    return formatElapsed(run.StartedAt, now)
  }, [run, now])

  if (loading) {
    return (
      <div className="flex items-center justify-center min-h-[60vh]">
        <p className="text-[13px] text-text-tertiary">Loading run…</p>
      </div>
    )
  }

  // Order matters: a 5xx / network error leaves `run` null AND sets
  // `error`. Checking notFound first would mask the real failure
  // behind a misleading "Run not found".
  if (error) {
    return (
      <div className="flex flex-col items-center justify-center min-h-[60vh] gap-3">
        <p className="text-[13px] text-dismiss">Failed to load: {error}</p>
        <Link to={orgHref('/board')} className="text-[12px] text-accent hover:underline">
          ← Back to board
        </Link>
      </div>
    )
  }

  if (notFound || !run) {
    return (
      <div className="flex flex-col items-center justify-center min-h-[60vh] gap-3">
        <p className="text-[13px] text-text-tertiary">Run not found.</p>
        <Link to={orgHref('/board')} className="text-[12px] text-accent hover:underline">
          ← Back to board
        </Link>
      </div>
    )
  }

  const status = statusDisplay(run)
  const canTakeOver = run.Status === 'running' && !!run.SessionID
  const isActive = isActiveRun(run)
  const isTerminal =
    run.Status === 'failed' ||
    run.Status === 'cancelled' ||
    run.Status === 'task_unsolvable' ||
    run.Status === 'completed' ||
    run.Status === 'pending_approval'

  return (
    <div className="flex flex-col min-h-[calc(100vh-5rem)]">
      <TakeoverModal info={takeoverInfo} onClose={() => setTakeoverInfo(null)} />

      {/* Sticky header */}
      <div className="sticky top-0 z-20 bg-surface-raised/90 backdrop-blur-xl border-b border-border-glass px-6 py-3">
        <div className="flex items-center gap-3 flex-wrap">
          <Link
            to={orgHref('/board')}
            aria-label="Back to board"
            className="shrink-0 inline-flex items-center justify-center w-8 h-8 rounded-lg text-text-tertiary hover:text-text-primary hover:bg-black/[0.04] transition-colors"
            title="Back to board (Esc)"
          >
            <ArrowLeft size={16} />
          </Link>

          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 mb-0.5">
              <span className={`text-[11px] font-semibold ${status.color}`}>
                {status.icon} {status.label}
              </span>
              {isActive && (
                <span className="inline-block w-1.5 h-1.5 rounded-full bg-delegate animate-pulse" />
              )}
              <span className="text-[11px] text-text-tertiary">{elapsed}</span>
            </div>
            <h1 className="text-[15px] font-semibold text-text-primary truncate">
              {task?.title || 'Untitled task'}
            </h1>
            {task && (
              <div className="flex items-center gap-2 text-[11px] text-text-tertiary mt-0.5">
                <SourceBadge task={task} />
                <span className="truncate">{task.source_id}</span>
                {task.source_url && (
                  <a
                    href={task.source_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-accent hover:underline"
                  >
                    Open source ↗
                  </a>
                )}
              </div>
            )}
          </div>

          {/* View mode toggle */}
          <div
            role="tablist"
            aria-label="Transcript view mode"
            className="inline-flex items-center rounded-lg border border-border-subtle bg-black/[0.02] p-0.5 shrink-0"
          >
            <ModeButton
              active={mode === 'conversation'}
              onClick={() => setMode('conversation')}
              shortcut="1"
            >
              Conversation
            </ModeButton>
            <ModeButton
              active={mode === 'commands'}
              onClick={() => setMode('commands')}
              shortcut="2"
            >
              Commands
            </ModeButton>
          </div>

          {/* Actions */}
          <div className="flex items-center gap-2 shrink-0">
            {canTakeOver && (
              <button
                disabled={takeoverPending}
                onClick={handleTakeover}
                className="text-[11px] font-semibold uppercase tracking-wider px-2.5 py-1 rounded-md text-accent bg-accent/10 hover:bg-accent/20 disabled:cursor-wait transition-colors"
                title="Stop the headless run and resume in your terminal (t)"
              >
                {takeoverPending ? 'Taking over…' : 'Take over'}
              </button>
            )}
            {isActive && (
              <button
                onClick={handleCancel}
                className="text-[11px] font-semibold uppercase tracking-wider px-2.5 py-1 rounded-md text-dismiss/80 hover:text-dismiss hover:bg-dismiss/10 transition-colors"
              >
                Cancel
              </button>
            )}
            {isTerminal && (
              <button
                onClick={handleRequeue}
                className="text-[11px] font-medium text-text-secondary hover:text-text-primary"
              >
                Return to queue
              </button>
            )}
          </div>
        </div>

        {chainSteps && chainSteps.length > 1 && (
          <div className="mt-3 max-w-md">
            <ChainStepsRail steps={chainSteps} currentRunID={run.ID} linkable />
          </div>
        )}
      </div>

      {/* Body */}
      <div className="flex flex-1 min-h-0">
        <div className="relative flex-1 min-w-0">
          <div
            ref={scrollRef}
            onScroll={onScroll}
            className="absolute inset-0 overflow-y-auto px-6 py-4"
            tabIndex={0}
          >
            <div ref={contentRef}>
              <Transcript messages={messages} run={run} mode={mode} />
            </div>
          </div>
          {!pinnedRef.current && (
            <button
              type="button"
              onClick={() => {
                const el = scrollRef.current
                if (el) el.scrollTop = el.scrollHeight
                setPinned(true)
              }}
              className="absolute bottom-4 left-1/2 -translate-x-1/2 inline-flex items-center gap-1.5 px-3 py-1.5 rounded-full bg-accent text-white text-[11px] font-semibold uppercase tracking-wider shadow-lg shadow-black/15 hover:bg-accent/90 transition-colors"
            >
              <svg width="12" height="12" viewBox="0 0 16 16" fill="none" aria-hidden>
                <path
                  d="M8 3v9m0 0l-4-4m4 4l4-4"
                  stroke="currentColor"
                  strokeWidth="1.8"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
              Jump to latest
            </button>
          )}
        </div>

        <aside className="w-[300px] shrink-0 border-l border-border-subtle bg-black/[0.01] overflow-y-auto">
          <SidePanel run={run} messages={messages} />
        </aside>
      </div>
    </div>
  )
}

function ModeButton({
  active,
  onClick,
  shortcut,
  children,
}: {
  active: boolean
  onClick: () => void
  shortcut?: string
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active}
      onClick={onClick}
      title={shortcut ? `Shortcut: ${shortcut}` : undefined}
      className={`text-[11px] font-medium px-2.5 py-1 rounded-md transition-colors ${
        active
          ? 'bg-surface-raised text-text-primary shadow-sm'
          : 'text-text-tertiary hover:text-text-secondary'
      }`}
    >
      {children}
    </button>
  )
}

function SidePanel({
  run,
  messages,
}: {
  run: AgentRun
  messages: import('../types').AgentMessage[]
}) {
  const tokens = useMemo(() => {
    let inT = 0
    let outT = 0
    let cacheR = 0
    let cacheW = 0
    for (const m of messages) {
      inT += m.InputTokens ?? 0
      outT += m.OutputTokens ?? 0
      cacheR += m.CacheReadTokens ?? 0
      cacheW += m.CacheCreationTokens ?? 0
    }
    return { inT, outT, cacheR, cacheW }
  }, [messages])

  const startedAt = run.StartedAt ? new Date(run.StartedAt) : null

  return (
    <div className="p-4 space-y-4 text-[12px]">
      <Section title="Run">
        <Field label="Status" value={run.Status} />
        {run.Model && <Field label="Model" value={run.Model} mono />}
        {startedAt && (
          <Field
            label="Started"
            value={`${startedAt.toLocaleString()} (${formatElapsed(run.StartedAt)} ago)`}
          />
        )}
        {run.DurationMs != null && run.DurationMs > 0 && (
          <Field label="Duration" value={formatDurationMs(run.DurationMs)} />
        )}
        {run.NumTurns != null && run.NumTurns > 0 && (
          <Field label="Turns" value={String(run.NumTurns)} />
        )}
        {run.StopReason && <Field label="Stop reason" value={run.StopReason} mono />}
        {run.TotalCostUSD != null && run.TotalCostUSD > 0 && (
          <Field label="Cost" value={`$${run.TotalCostUSD.toFixed(4)}`} />
        )}
      </Section>

      {(tokens.inT > 0 || tokens.outT > 0) && (
        <Section title="Tokens">
          <Field label="Input" value={tokens.inT.toLocaleString()} mono />
          <Field label="Output" value={tokens.outT.toLocaleString()} mono />
          {tokens.cacheR > 0 && (
            <Field label="Cache read" value={tokens.cacheR.toLocaleString()} mono />
          )}
          {tokens.cacheW > 0 && (
            <Field label="Cache write" value={tokens.cacheW.toLocaleString()} mono />
          )}
        </Section>
      )}

      {(run.SessionID || run.WorktreePath) && (
        <Section title="Session">
          {run.SessionID && <Copyable label="Session ID" value={run.SessionID} />}
          {run.WorktreePath && <Copyable label="Worktree" value={run.WorktreePath} />}
        </Section>
      )}

      <Section title="Identifiers">
        <Copyable label="Run ID" value={run.ID} />
        {run.TaskID && <Copyable label="Task ID" value={run.TaskID} />}
      </Section>
    </div>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-[10px] font-semibold uppercase tracking-wider text-text-tertiary mb-2">
        {title}
      </div>
      <div className="space-y-1.5">{children}</div>
    </div>
  )
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-start justify-between gap-3">
      <span className="text-text-tertiary shrink-0">{label}</span>
      <span
        className={`text-text-primary text-right min-w-0 break-words ${mono ? 'font-mono text-[11px]' : ''}`}
      >
        {value}
      </span>
    </div>
  )
}

function Copyable({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1200)
    } catch (err) {
      toast.error(`Failed to copy: ${(err as Error).message}`)
    }
  }
  return (
    <div>
      <div className="text-text-tertiary text-[10px] mb-0.5">{label}</div>
      <div className="flex items-stretch rounded-md border border-border-subtle bg-black/[0.02]">
        <div className="flex-1 min-w-0 px-2 py-1 font-mono text-[10.5px] text-text-primary break-all">
          {value}
        </div>
        <button
          onClick={onCopy}
          className="shrink-0 px-2 text-[10px] font-medium text-text-secondary hover:text-text-primary border-l border-border-subtle hover:bg-black/[0.04] transition-colors"
        >
          {copied ? '✓' : 'Copy'}
        </button>
      </div>
    </div>
  )
}
