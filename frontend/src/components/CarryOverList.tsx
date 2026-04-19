import { useEffect, useState, useCallback, useRef } from 'react'
import { ExternalLink, RotateCw } from 'lucide-react'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'

type Action = 'queue' | 'claim' | 'done'

interface StockTicket {
  issue_key: string
  summary: string
  status: string
  project: string
  issue_type: string
  priority: string
  parent_key?: string
  parent_url?: string
  url: string
  already_done?: boolean
}

interface Props {
  onSave: () => void
  onSkip: () => void
  onBack: () => void
}

const POLL_INTERVAL_MS = 1500

export default function CarryOverList({ onSave, onSkip, onBack }: Props) {
  const [tickets, setTickets] = useState<StockTicket[] | null>(null)
  const [polling, setPolling] = useState(true)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const [selections, setSelections] = useState<Record<string, Action>>({})
  // Per-row error messages from the last POST. Cleared when the user changes a
  // selection on that row so stale errors don't linger after a retry attempt.
  const [failures, setFailures] = useState<Record<string, string>>({})

  // Keep refs for component lifecycle and polling so retry timers don't
  // continue firing after unmount.
  const mountedRef = useRef(true)
  const pollTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      if (pollTimeoutRef.current) {
        clearTimeout(pollTimeoutRef.current)
        pollTimeoutRef.current = null
      }
    }
  }, [])

  const fetchStock = useCallback(async () => {
    try {
      const res = await fetch('/api/jira/stock')
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        console.error('carry-over fetch failed:', data.error || `HTTP ${res.status}`)
        if (mountedRef.current) {
          setError('Failed to load tickets')
          setPolling(false)
        }
        return
      }
      const data = await res.json()
      if (!mountedRef.current) return
      if (data.status === 'polling') {
        setPolling(true)
        if (pollTimeoutRef.current) {
          clearTimeout(pollTimeoutRef.current)
        }
        pollTimeoutRef.current = setTimeout(() => {
          pollTimeoutRef.current = null
          fetchStock()
        }, POLL_INTERVAL_MS)
        return
      }
      const fetched: StockTicket[] = data.tickets || []
      setTickets(fetched)
      // Pre-select "done" for tickets already in any configured Done.Members
      // status — one-click cleanup of orphan entities. User can still
      // deselect or change the action before saving.
      setSelections((prev) => {
        const next = { ...prev }
        for (const t of fetched) {
          if (t.already_done && next[t.issue_key] === undefined) {
            next[t.issue_key] = 'done'
          }
        }
        return next
      })
      setPolling(false)
      setError('')
    } catch (err) {
      console.error('carry-over fetch failed:', err)
      if (mountedRef.current) {
        setError('Failed to load tickets')
        setPolling(false)
      }
    }
  }, [])

  useEffect(() => {
    fetchStock()
  }, [fetchStock])

  const toggle = (issueKey: string, action: Action) => {
    setSelections((prev) => {
      const next = { ...prev }
      if (next[issueKey] === action) {
        delete next[issueKey]
      } else {
        next[issueKey] = action
      }
      return next
    })
    // Changing a selection invalidates any prior failure message on this row.
    setFailures((prev) => {
      if (!prev[issueKey]) return prev
      const next = { ...prev }
      delete next[issueKey]
      return next
    })
  }

  const selectionCount = Object.keys(selections).length

  const handleSave = async () => {
    if (selectionCount === 0) return
    setSaving(true)
    try {
      const actions = Object.entries(selections).map(([issue_key, action]) => ({
        issue_key,
        action,
      }))
      const res = await fetch('/api/jira/stock', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ actions }),
      })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to save carry-over selections'))
        return
      }
      const body = (await res.json()) as {
        applied: number
        failed?: { issue_key: string; action: string; error: string }[]
      }
      const failedList = body.failed ?? []
      if (failedList.length === 0) {
        onSave()
        return
      }

      // Partial success: remove successfully applied rows from the list, keep
      // failed rows visible with inline error messages. The user can change or
      // retry those selections, or skip to continue. Surface a summary toast
      // so the partial nature is obvious even if the failing rows scroll off.
      toast.warning(
        `Applied ${body.applied} ticket${body.applied === 1 ? '' : 's'}; ${failedList.length} failed — see inline errors`,
      )
      const failedKeys = new Set(failedList.map((f) => f.issue_key))
      const appliedKeys = new Set(Object.keys(selections).filter((k) => !failedKeys.has(k)))
      setTickets((prev) => (prev ?? []).filter((t) => !appliedKeys.has(t.issue_key)))
      setSelections((prev) => {
        const next: Record<string, Action> = {}
        for (const [k, v] of Object.entries(prev)) {
          if (!appliedKeys.has(k)) next[k] = v
        }
        return next
      })
      setFailures(
        failedList.reduce<Record<string, string>>((acc, f) => {
          acc[f.issue_key] = f.error
          return acc
        }, {}),
      )
    } catch (err) {
      toast.error(`Failed to save carry-over: ${(err as Error).message}`)
    } finally {
      if (mountedRef.current) setSaving(false)
    }
  }

  return (
    <div className="w-full max-w-2xl backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.04] overflow-hidden flex flex-col max-h-[85vh]">
      {/* Header */}
      <div className="px-6 pt-6 pb-4">
        <h2 className="text-[18px] font-semibold text-text-primary tracking-tight">Carry over</h2>
        <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
          Decide what to do with tickets already assigned to you. Queue for triage, claim as active,
          or mark already-complete tickets done. Leave rows unselected to skip.
        </p>
      </div>

      {/* Body */}
      <div className="flex-1 overflow-y-auto px-6 min-h-0">
        {polling && tickets === null && (
          <div className="space-y-1 py-2">
            <p className="text-[12px] text-text-tertiary text-center pb-2">
              Fetching your tickets…
            </p>
            {[1, 2, 3, 4, 5].map((i) => (
              <div key={i} className="flex items-center gap-3 px-3 py-2.5 rounded-xl">
                <div className="flex-1 space-y-1.5">
                  <div
                    className="h-3 rounded bg-black/[0.04] animate-pulse"
                    style={{ width: `${55 + ((i * 17) % 35)}%` }}
                  />
                  <div
                    className="h-2.5 rounded bg-black/[0.03] animate-pulse"
                    style={{ width: `${30 + ((i * 23) % 40)}%` }}
                  />
                </div>
                <div className="w-[180px] h-6 rounded-full bg-black/[0.04] animate-pulse" />
              </div>
            ))}
          </div>
        )}

        {error && !polling && (
          <div className="flex flex-col items-center justify-center py-12 gap-3">
            <div className="text-[13px] text-text-secondary text-center">{error}</div>
            <button
              type="button"
              onClick={() => {
                setError('')
                setPolling(true)
                setTickets(null)
                fetchStock()
              }}
              className="flex items-center gap-1.5 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors"
            >
              <RotateCw size={13} />
              Retry
            </button>
          </div>
        )}

        {!polling && !error && tickets && tickets.length === 0 && (
          <p className="text-[13px] text-text-tertiary text-center py-12">
            No existing work to carry over.
          </p>
        )}

        {!polling && !error && tickets && tickets.length > 0 && (
          <>
            {Object.keys(failures).length > 0 && (
              <div className="mb-2 rounded-xl bg-dismiss/[0.06] border border-dismiss/20 px-3 py-2 text-[12px] text-dismiss">
                Some actions couldn&rsquo;t be applied. Successful rows were saved; fix the
                highlighted rows below or skip to continue.
              </div>
            )}
            <div className="py-2 space-y-0.5">
              {tickets.map((t) => (
                <TicketRow
                  key={t.issue_key}
                  ticket={t}
                  selection={selections[t.issue_key]}
                  failure={failures[t.issue_key]}
                  onToggle={(action) => toggle(t.issue_key, action)}
                />
              ))}
            </div>
          </>
        )}
      </div>

      {/* Footer */}
      <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between">
        <div className="flex items-center gap-3">
          <button
            type="button"
            onClick={onBack}
            className="text-[13px] text-text-secondary hover:text-text-primary bg-white/50 hover:bg-white/80 border border-border-subtle rounded-xl px-4 py-2 transition-colors"
          >
            Back
          </button>
          <button
            type="button"
            onClick={onSkip}
            className="text-[12px] text-text-tertiary hover:text-text-secondary transition-colors"
          >
            Skip for now
          </button>
        </div>
        <div className="flex items-center gap-3">
          <span className="text-[11px] text-text-tertiary">{selectionCount} selected</span>
          <button
            type="button"
            onClick={handleSave}
            disabled={selectionCount === 0 || saving}
            className="bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-5 py-2 text-[13px] transition-colors"
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>
    </div>
  )
}

function TicketRow({
  ticket,
  selection,
  failure,
  onToggle,
}: {
  ticket: StockTicket
  selection: Action | undefined
  failure?: string
  onToggle: (action: Action) => void
}) {
  return (
    <div
      className={`flex items-start gap-3 px-3 py-2.5 rounded-xl transition-colors ${
        failure ? 'bg-dismiss/[0.04] border border-dismiss/20' : 'hover:bg-black/[0.02]'
      }`}
    >
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <a
            href={ticket.url}
            target="_blank"
            rel="noopener noreferrer"
            onClick={(e) => e.stopPropagation()}
            className="text-[11px] font-medium text-accent hover:text-accent/80 bg-accent/[0.08] hover:bg-accent/[0.12] rounded px-1.5 py-0.5 transition-colors inline-flex items-center gap-1 shrink-0"
          >
            {ticket.issue_key}
            <ExternalLink size={10} />
          </a>
          <span className="text-[13px] font-medium text-text-primary truncate">
            {ticket.summary}
          </span>
          {ticket.already_done && (
            <span className="shrink-0 text-[10px] text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5">
              already {ticket.status}
            </span>
          )}
        </div>
        <MetadataLine ticket={ticket} />
        {failure && (
          <div className="mt-1 text-[11px] text-dismiss" title={failure}>
            {failure}
          </div>
        )}
      </div>

      <TriSelector selection={selection} onToggle={onToggle} />
    </div>
  )
}

// MetadataLine renders ticket metadata in the order: status · priority ·
// issue_type · parent. Separators are inserted only between present values
// so trailing/leading dots never appear. Status is hidden when already_done
// is showing it via the trailing pill on the first line.
//
// Each part carries its own stable key ("status" / "priority" / ...) so
// React reconciliation doesn't mis-match nodes when visibility changes (e.g.
// a ticket flipping already_done causes status to drop out of the list, and
// index keys would then shift "priority" into "status"'s slot).
function MetadataLine({ ticket }: { ticket: StockTicket }) {
  const parts: { key: string; node: React.ReactNode }[] = []
  if (ticket.status && !ticket.already_done) {
    parts.push({
      key: 'status',
      node: <span className="text-text-secondary font-medium">{ticket.status}</span>,
    })
  }
  if (ticket.priority) {
    parts.push({ key: 'priority', node: <span>{ticket.priority}</span> })
  }
  if (ticket.issue_type) {
    parts.push({ key: 'type', node: <span>{ticket.issue_type}</span> })
  }
  if (ticket.parent_key && ticket.parent_url) {
    parts.push({
      key: 'parent',
      node: (
        <a
          href={ticket.parent_url}
          target="_blank"
          rel="noopener noreferrer"
          className="hover:text-accent transition-colors"
        >
          {ticket.parent_key}
        </a>
      ),
    })
  }

  return (
    <div className="flex items-center gap-1.5 mt-0.5 text-[11px] text-text-tertiary">
      {parts.length === 0 ? (
        <span className="italic">no metadata</span>
      ) : (
        parts.map((p, i) => (
          <span key={p.key} className="flex items-center gap-1.5">
            {i > 0 && <span>·</span>}
            {p.node}
          </span>
        ))
      )}
    </div>
  )
}

function TriSelector({
  selection,
  onToggle,
}: {
  selection: Action | undefined
  onToggle: (action: Action) => void
}) {
  const options: { value: Action; label: string }[] = [
    { value: 'queue', label: 'Queue' },
    { value: 'claim', label: 'Claim' },
    { value: 'done', label: 'Done' },
  ]
  return (
    <div className="flex rounded-full border border-border-subtle p-0.5 gap-0.5 shrink-0">
      {options.map((o) => {
        const active = selection === o.value
        return (
          <button
            key={o.value}
            type="button"
            onClick={() => onToggle(o.value)}
            className={
              active
                ? 'px-3 py-1 rounded-full bg-accent text-white text-[11px] font-medium transition-colors'
                : 'px-3 py-1 rounded-full text-text-tertiary hover:text-text-secondary text-[11px] transition-colors'
            }
          >
            {o.label}
          </button>
        )
      })}
    </div>
  )
}
