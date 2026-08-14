// FailedEventsPanel — the diagnostics table over parked event_queue rows, and
// the requeue that puts them back.
//
// What a row here means: the event is durably recorded, but its routing
// obligations — task mint/bump, owner resolution, trigger firing, close
// processing — exhausted the retry budget and will never run. Nothing
// re-drives it on its own, because the tracker's snapshot advanced when the
// event was minted and the transition will not re-emit. So this table is the
// whole recovery path, and an empty one is the healthy state.
//
// Requeue is safe to offer as a plain button: a requeued row re-enters the
// exact at-least-once path every retry already uses (task upsert under the
// dedup index, firing behind the replay fences), so double-clicking it, or
// requeueing an event whose task has since arrived another way, converges to
// no-ops. The backend counts out ids that are no longer parked, which is why
// "Requeued 2 of 3" is a normal outcome rather than an error.
//
// Admin-only by placement: this renders inside OrgSettings, which multi mode
// only mounts on the org-admin Settings tab and local mode (N=1, admin of
// everything) always mounts. That mirrors the API gate, which 403s a plain
// member on both endpoints.

import { useState } from 'react'
import { RotateCcw } from 'lucide-react'
import { toast } from '../../components/Toast/toastStore'
import { httpErrorMessage } from '../../lib/apiClient'
import { timeAgo } from '../../lib/relativeTime'
import type { FailedEvent, UseFailedEvents } from '../../hooks/useFailedEvents'

// entityLabel renders the row's entity the way an operator recognizes it: the
// source id (a GitHub "owner/repo#18", a Jira issue key) with the title as
// context. Both can be absent — a queue row can carry no entity — so the
// source id is the floor and the raw entity id the fallback under it.
function entityLabel(e: FailedEvent): string {
  if (e.entity_source_id) return e.entity_source_id
  if (e.entity_id) return e.entity_id
  return '—'
}

export default function FailedEventsPanel({ state }: { state: UseFailedEvents }) {
  const { events, loading, error, reload, requeue } = state
  // Selected ids, and the ids with a requeue in flight. Two sets rather than
  // one busy flag so a per-row requeue disables only its own button while a
  // select-all requeue disables every row it covers.
  const [selected, setSelected] = useState<Set<number>>(() => new Set())
  const [inFlight, setInFlight] = useState<Set<number>>(() => new Set())

  const allSelected = events.length > 0 && selected.size === events.length
  const toggleRow = (id: number) =>
    setSelected((s) => {
      const n = new Set(s)
      if (n.has(id)) n.delete(id)
      else n.add(id)
      return n
    })
  const toggleAll = () => setSelected(allSelected ? new Set() : new Set(events.map((e) => e.id)))

  const run = async (ids: number[]) => {
    if (ids.length === 0) return
    setInFlight((s) => new Set([...s, ...ids]))
    try {
      const moved = await requeue(ids)
      // The requested-vs-moved split is the honest report: the difference is
      // rows that stopped being parked between the page load and the click.
      if (moved === ids.length) {
        toast.success(`Requeued ${moved} event${moved === 1 ? '' : 's'}`)
      } else {
        toast.info(`Requeued ${moved} of ${ids.length} — the rest were no longer parked`)
      }
      // reload already ran inside requeue; drop the ids it consumed so a
      // stale selection can't be re-submitted.
      setSelected((s) => {
        const n = new Set(s)
        ids.forEach((id) => n.delete(id))
        return n
      })
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not requeue those events.'))
    } finally {
      setInFlight((s) => {
        const n = new Set(s)
        ids.forEach((id) => n.delete(id))
        return n
      })
    }
  }

  if (loading) {
    return <p className="text-body text-ink-3">Loading parked events…</p>
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
  if (events.length === 0) {
    return (
      <div className="space-y-2">
        <p className="text-body text-ink-1">Nothing parked.</p>
        <p className="text-reported leading-relaxed text-ink-3">
          Events land here only when routing fails repeatedly enough to exhaust the retry budget. An
          empty list is the healthy state.
        </p>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          Events that never routed
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          Each of these was recorded but its follow-up work — creating or bumping a task, firing a
          trigger, processing a close — failed enough times to be parked. Nothing retries them
          automatically, because the poller only reports transitions once. Requeue puts a row back
          on the queue with a fresh budget; doing it twice is harmless.
        </p>
      </div>

      <div className="flex items-center justify-between gap-4">
        <label className="flex items-center gap-2 text-ui text-ink-2">
          <input
            type="checkbox"
            checked={allSelected}
            onChange={toggleAll}
            aria-label="Select all parked events"
            className="h-3.5 w-3.5 accent-[var(--color-warm)]"
          />
          Select all ({events.length})
        </label>
        <button
          type="button"
          onClick={() => void run([...selected])}
          disabled={selected.size === 0 || inFlight.size > 0}
          className="shrink-0 rounded-xl border border-warm/20 px-4 py-2 text-body text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
        >
          Requeue selected{selected.size > 0 ? ` (${selected.size})` : ''}
        </button>
      </div>

      {/* The error column is the widest and the least predictable, so the table
          scrolls inside its own box rather than widening the settings stack. */}
      <div className="overflow-x-auto rounded-xl border border-line-1">
        <table className="w-full min-w-[720px] text-left text-ui">
          <thead className="border-b border-line-1 text-reported text-ink-3">
            <tr>
              <th scope="col" className="w-8 px-3 py-2">
                <span className="sr-only">Select</span>
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Event
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Entity
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Age
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Attempts
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                Last error
              </th>
              <th scope="col" className="px-3 py-2 font-normal">
                <span className="sr-only">Requeue</span>
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line-1">
            {events.map((e) => {
              const busy = inFlight.has(e.id)
              return (
                <tr key={e.id} className="align-top">
                  <td className="px-3 py-2">
                    <input
                      type="checkbox"
                      checked={selected.has(e.id)}
                      onChange={() => toggleRow(e.id)}
                      disabled={busy}
                      aria-label={`Select event ${e.id}`}
                      className="h-3.5 w-3.5 accent-[var(--color-warm)]"
                    />
                  </td>
                  {/* The raw event type, not a friendly label: this is the
                      string an operator greps the logs and the trigger config
                      for. */}
                  <td className="px-3 py-2 font-mono text-reported text-ink-1">
                    {e.event_type}
                  </td>
                  <td className="px-3 py-2 text-ink-2">
                    <span className="font-mono text-reported">{entityLabel(e)}</span>
                    {e.entity_title && (
                      <span className="mt-0.5 block max-w-[220px] truncate text-reported text-ink-3">
                        {e.entity_title}
                      </span>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-ink-3">
                    {timeAgo(e.enqueued_at)}
                  </td>
                  <td className="px-3 py-2 text-ink-3">{e.attempts}</td>
                  <td className="px-3 py-2">
                    <span className="block max-w-[320px] break-words font-mono text-reported text-ink-2">
                      {e.last_error || '—'}
                    </span>
                  </td>
                  <td className="px-3 py-2 text-right">
                    <button
                      type="button"
                      onClick={() => void run([e.id])}
                      disabled={busy}
                      title="Requeue this event"
                      className="inline-flex items-center gap-1 rounded-lg border border-line-1 px-2 py-1 text-reported text-ink-2 transition-colors hover:border-warm/30 hover:text-warm disabled:opacity-40"
                    >
                      <RotateCcw size={11} aria-hidden />
                      {busy ? 'Requeueing…' : 'Requeue'}
                    </button>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}
