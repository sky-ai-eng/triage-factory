// ParkedWorkPanel — the operator table over durable work across every
// registered kind, and the controls that move it: redrive for a parked row,
// a cancellation request for a ready or leased one.
//
// What a parked row means: the work is durably recorded, but its attempts
// exhausted the retry budget or failed permanently, and it will never run on
// its own. So this table is the whole recovery path, and an empty one is the
// healthy state. The status filter is what lets an operator see the rest of a
// queue — a poison row still retrying, a row somebody is driving right now —
// and stop it before it parks.
//
// Redrive is safe to offer as a plain button: a redriven row re-enters the
// exact at-least-once path every retry already uses, so double-clicking it,
// or redriving work that has since been done another way, converges to
// no-ops. Cancel records a request, not a status change: the row settles at
// its next claim or its holder's next renewal, which is what lets a request
// land safely against a row somebody is mid-way through executing. The
// backend counts out ids that no longer qualify, which is why "Redrove 2 of
// 3" is a normal outcome rather than an error.
//
// Admin-only by placement: this renders inside OrgSettings, which multi mode
// only mounts on the org-admin Settings tab and local mode (N=1, admin of
// everything) always mounts. That mirrors the API gate, which 403s a plain
// member on every route.

import { useState } from 'react'
import { RotateCcw, XCircle } from 'lucide-react'
import { toast } from '../../components/Toast/toastStore'
import { httpErrorMessage } from '../../lib/apiClient'
import { timeAgo } from '../../lib/relativeTime'
import type { UseParkedWork, WorkItem, WorkStatusFilter } from '../../hooks/useParkedWork'

const STATUS_FILTERS: { value: WorkStatusFilter; label: string }[] = [
  { value: 'parked', label: 'Parked' },
  { value: 'ready', label: 'Ready' },
  { value: 'leased', label: 'Leased' },
  { value: 'deferred', label: 'Deferred' },
]

// rowKey is the selection identity: ids are per kind, so a bare id is not
// unique across the table.
function rowKey(it: WorkItem): string {
  return `${it.kind}:${it.id}`
}

// ParkedWorkBadge is the Parked work section's collapsed summary. Zero is
// the healthy state and reads as plain text; anything above zero gets a
// warning-toned count pill, because a parked row is work that will not come
// back on its own. The number is the sum of every kind's parked depth — the
// org's whole population, not the length of a page.
export function ParkedWorkBadge({ state }: { state: UseParkedWork }) {
  if (state.loading || state.error) return <>Parked work</>
  const n = state.parkedTotal
  if (n === 0) return <>None parked</>
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="rounded-full bg-alarm/10 px-2 py-0.5 text-reported font-medium text-alarm">
        {n}
      </span>
      parked
    </span>
  )
}

export default function ParkedWorkPanel({ state }: { state: UseParkedWork }) {
  const {
    kinds,
    items,
    total,
    status,
    setStatus,
    loading,
    error,
    hasMore,
    loadMore,
    reload,
    redrive,
    cancel,
  } = state
  // Selected row keys, and the keys with a control in flight. Two sets rather
  // than one busy flag so a per-row control disables only its own button
  // while a select-all redrive disables every row it covers.
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const [inFlight, setInFlight] = useState<Set<string>>(() => new Set())
  // The row whose cancel form is open, and the reason typed into it.
  const [cancelling, setCancelling] = useState<string | null>(null)
  const [reason, setReason] = useState('')

  const controlsOf = (kind: string) => kinds.find((k) => k.kind === kind)?.controls
  const redrivable = items.filter((it) => it.status === 'parked' && controlsOf(it.kind)?.redrive)
  const allSelected = redrivable.length > 0 && redrivable.every((it) => selected.has(rowKey(it)))
  const toggleRow = (key: string) =>
    setSelected((s) => {
      const n = new Set(s)
      if (n.has(key)) n.delete(key)
      else n.add(key)
      return n
    })
  const toggleAll = () =>
    setSelected(allSelected ? new Set() : new Set(redrivable.map((it) => rowKey(it))))

  const markInFlight = (keys: string[], on: boolean) =>
    setInFlight((s) => {
      const n = new Set(s)
      keys.forEach((k) => (on ? n.add(k) : n.delete(k)))
      return n
    })

  // runRedrive groups the selection by kind — ids are per kind — and reports
  // the requested-vs-moved split, which is the honest report: the difference
  // is rows that stopped being parked between the page load and the click.
  const runRedrive = async (rows: WorkItem[]) => {
    if (rows.length === 0) return
    const keys = rows.map(rowKey)
    markInFlight(keys, true)
    try {
      const byKind = new Map<string, number[]>()
      rows.forEach((it) => byKind.set(it.kind, [...(byKind.get(it.kind) ?? []), it.id]))
      let moved = 0
      for (const [kind, ids] of byKind) {
        moved += await redrive(kind, ids)
      }
      if (moved === rows.length) {
        toast.success(`Redrove ${moved} item${moved === 1 ? '' : 's'}`)
      } else {
        toast.info(`Redrove ${moved} of ${rows.length} — the rest were no longer parked`)
      }
      setSelected((s) => {
        const n = new Set(s)
        keys.forEach((k) => n.delete(k))
        return n
      })
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not redrive those items.'))
    } finally {
      markInFlight(keys, false)
    }
  }

  const runCancel = async (it: WorkItem) => {
    const trimmed = reason.trim()
    if (trimmed === '') return
    const key = rowKey(it)
    markInFlight([key], true)
    try {
      const requested = await cancel(it.kind, [it.id], trimmed)
      if (requested === 1) {
        toast.success('Cancellation requested — the row settles at its next claim or renewal')
      } else {
        toast.info('Nothing to cancel — the row had already settled or been requested')
      }
      setCancelling(null)
      setReason('')
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not request that cancellation.'))
    } finally {
      markInFlight([key], false)
    }
  }

  const filter = (
    <div className="flex flex-wrap items-center gap-2" role="group" aria-label="Status filter">
      {STATUS_FILTERS.map((f) => (
        <button
          key={f.value}
          type="button"
          aria-pressed={status === f.value}
          onClick={() => {
            setSelected(new Set())
            setCancelling(null)
            setStatus(f.value)
          }}
          className={`rounded-lg border px-3 py-1 text-reported transition-colors ${
            status === f.value
              ? 'border-warm/40 text-warm'
              : 'border-line-1 text-ink-3 hover:text-ink-2'
          }`}
        >
          {f.label}
        </button>
      ))}
    </div>
  )

  if (loading && items.length === 0) {
    return <p className="text-body text-ink-3">Loading parked work…</p>
  }
  if (error) {
    return (
      <p className="text-body text-ink-2">
        {error}{' '}
        <button type="button" onClick={() => void reload()} className="text-warm underline">
          Retry
        </button>
      </p>
    )
  }
  if (items.length === 0) {
    return (
      <div className="space-y-3">
        {filter}
        {status === 'parked' ? (
          <>
            <p className="text-body text-ink-1">Nothing parked.</p>
            <p className="text-reported leading-relaxed text-ink-3">
              Work lands here only when it fails repeatedly enough to exhaust its retry budget, or
              fails permanently. An empty list is the healthy state.
            </p>
          </>
        ) : (
          <p className="text-body text-ink-1">No {status} work.</p>
        )}
      </div>
    )
  }

  const showKind = kinds.length > 1

  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Work that stopped</h2>
        <p className="text-body leading-relaxed text-ink-3">
          A parked row was recorded but its attempts failed enough times to stop. Nothing retries it
          automatically. Redrive puts it back with a fresh budget; doing it twice is harmless.
          Cancel records a request against a row that is still ready or being driven — it takes
          effect at the row&apos;s next claim or renewal, not immediately.
        </p>
      </div>

      {filter}

      <div className="flex items-center justify-between gap-4">
        <label className="flex items-center gap-2 text-ui text-ink-2">
          <input
            type="checkbox"
            checked={allSelected}
            onChange={toggleAll}
            disabled={redrivable.length === 0}
            aria-label="Select all parked work"
            className="h-3.5 w-3.5 accent-[var(--color-warm)]"
          />
          Select all ({redrivable.length})
          {total > items.length && (
            <span className="text-ink-3">
              {' '}
              of {total} {status}
            </span>
          )}
        </label>
        <button
          type="button"
          onClick={() => void runRedrive(redrivable.filter((it) => selected.has(rowKey(it))))}
          disabled={selected.size === 0 || inFlight.size > 0}
          className="shrink-0 rounded-xl border border-warm/20 px-4 py-2 text-body text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
        >
          Redrive selected{selected.size > 0 ? ` (${selected.size})` : ''}
        </button>
      </div>

      {/* The error column is the widest and the least predictable, so the table
          scrolls inside its own box rather than widening the settings stack. */}
      <div className="overflow-x-auto rounded-xl border border-line-1">
        <table className="w-full min-w-[760px] text-left text-ui">
          <thead className="border-b border-line-1 text-reported text-ink-3">
            <tr>
              <th scope="col" className="w-8 px-3 py-2">
                <span className="sr-only">Select</span>
              </th>
              {showKind && (
                <th scope="col" className="px-3 py-2 font-normal">
                  Kind
                </th>
              )}
              <th scope="col" className="px-3 py-2 font-normal">
                Subject
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Outcome
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Attempts
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Age
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Error
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-1">
            {items.map((it) => {
              const key = rowKey(it)
              const busy = inFlight.has(key)
              const controls = controlsOf(it.kind)
              const canRedrive = it.status === 'parked' && !!controls?.redrive
              const canCancel =
                (it.status === 'ready' || it.status === 'leased') &&
                !!controls?.cancel &&
                !it.cancel
              const kindLabel = kinds.find((k) => k.kind === it.kind)?.label ?? it.kind
              return (
                <tr key={key} className="align-top">
                  <td className="px-3 py-2">
                    <input
                      type="checkbox"
                      checked={selected.has(key)}
                      onChange={() => toggleRow(key)}
                      disabled={busy || !canRedrive}
                      aria-label={`Select ${it.kind} item ${it.id}`}
                      className="h-3.5 w-3.5 accent-[var(--color-warm)]"
                    />
                  </td>
                  {showKind && <td className="px-3 py-2 text-ink-2">{kindLabel}</td>}
                  <td className="px-3 py-2 text-ink-2">
                    {/* The kind's own label for the row — a repo#number, an
                        issue key — with its detail beneath. A row the kind
                        could not describe shows its bare id. */}
                    <span className="font-mono text-reported">
                      {it.subject?.label ?? `#${it.id}`}
                    </span>
                    {it.subject?.detail && (
                      <span className="mt-0.5 block max-w-[220px] truncate text-reported text-ink-3">
                        {it.subject.detail}
                      </span>
                    )}
                    {it.subject?.fields.event_type && (
                      <span className="mt-0.5 block font-mono text-reported text-ink-3">
                        {it.subject.fields.event_type}
                      </span>
                    )}
                  </td>
                  <td className="px-3 py-2 font-mono text-reported text-ink-2">
                    {it.last_outcome || '—'}
                    {it.cancel && (
                      <span className="mt-0.5 block text-reported text-ink-3">
                        cancel requested
                      </span>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-ink-3">
                    {it.attempt}/{it.max_attempts}
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-ink-3">
                    {timeAgo(it.first_enqueued_at)}
                  </td>
                  <td className="px-3 py-2">
                    <span className="block max-w-[320px] break-words font-mono text-reported text-ink-2">
                      {it.last_error || '—'}
                    </span>
                  </td>
                  <td className="px-3 py-2 text-right">
                    {canRedrive && (
                      <button
                        type="button"
                        onClick={() => void runRedrive([it])}
                        disabled={busy}
                        title="Redrive this item"
                        className="inline-flex items-center gap-1 rounded-lg border border-line-1 px-2 py-1 text-reported text-ink-2 transition-colors hover:border-warm/30 hover:text-warm disabled:opacity-40"
                      >
                        <RotateCcw size={11} aria-hidden />
                        {busy ? 'Redriving…' : 'Redrive'}
                      </button>
                    )}
                    {canCancel && cancelling !== key && (
                      <button
                        type="button"
                        onClick={() => {
                          setCancelling(key)
                          setReason('')
                        }}
                        disabled={busy}
                        title="Request cancellation of this item"
                        className="inline-flex items-center gap-1 rounded-lg border border-line-1 px-2 py-1 text-reported text-ink-2 transition-colors hover:border-alarm/40 hover:text-alarm disabled:opacity-40"
                      >
                        <XCircle size={11} aria-hidden />
                        Cancel…
                      </button>
                    )}
                    {canCancel && cancelling === key && (
                      <form
                        className="flex items-center gap-1.5"
                        onSubmit={(e) => {
                          e.preventDefault()
                          void runCancel(it)
                        }}
                      >
                        <input
                          type="text"
                          value={reason}
                          onChange={(e) => setReason(e.target.value)}
                          placeholder="Reason (required)"
                          aria-label={`Cancellation reason for ${it.kind} item ${it.id}`}
                          maxLength={500}
                          autoFocus
                          className="w-40 rounded-lg border border-line-1 bg-transparent px-2 py-1 text-reported text-ink-1"
                        />
                        <button
                          type="submit"
                          disabled={busy || reason.trim() === ''}
                          className="rounded-lg border border-alarm/40 px-2 py-1 text-reported text-alarm disabled:opacity-40"
                        >
                          {busy ? 'Requesting…' : 'Request cancel'}
                        </button>
                        <button
                          type="button"
                          onClick={() => setCancelling(null)}
                          className="px-1 text-reported text-ink-3"
                        >
                          Keep
                        </button>
                      </form>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {hasMore && (
        <button
          type="button"
          onClick={() => void loadMore()}
          disabled={loading}
          className="w-full rounded-xl border border-line-1 py-2 text-[12px] text-ink-3 transition-colors hover:text-ink-2 disabled:opacity-50"
        >
          {loading ? 'Loading…' : `Load more — showing ${items.length} of ${total}`}
        </button>
      )}
    </div>
  )
}
