import { useEffect, useMemo, useRef, useState } from 'react'
import type { Deliverable } from '../types'
import { httpErrorMessage } from '../lib/apiClient'
import { dismissDeliverable } from '../lib/deliverables'
import { usePagedList } from '../hooks/usePagedList'
import { toast } from './Toast/toastStore'
import { DeliverableRow } from './DeliverableRow'

// DeliverableList — a conversation's deliverables, and the approval surface the
// RunStation dock's popover raises. One row per deliverable: a kind icon + the
// target + a state badge + an external link. Paged conversation-scoped from
// POST /api/agent/conversations/{id}/deliverables/list; the component owns its
// own fetch so a consumer stays a one-liner, and mounts on open → lazy-fetch.
//
// When the owner passes the conversation projection's `pendingDeliverableIds` (the
// authoritative unresolved set — the ready-review predicate lives server-side,
// so deliverable.state alone can't reconstruct it), those rows sort to the top of
// the loaded pages in the projection's order (draft PRs first, then ready
// reviews) and carry an in-place [x] dismiss. Everything else keeps the backend's
// order below them.
//
// It is deliberately not the answer to "what did this conversation do" — a
// deliverable is work waiting on a person, and the objects a run changed are its
// artifacts, which the Run page's rail lists (RunArtifacts).
//
// The row rendering itself lives in DeliverableRow.tsx, shared with the
// bot-activity audit feed. pull_request / review rows open their approval
// overlays (PendingPROverlay / ReviewOverlay) by deliverable id via
// onOpenApproval — those overlays are owned by the parent (Board / RunDetail),
// so this list never duplicates them. Every other kind (branch / issue /
// comment), and any row when no handler is wired, just links out to `url`.
interface Props {
  conversationId: string
  onOpenApproval?: (kind: 'review' | 'pr', deliverableId: string) => void
  /** The conversation projection's authoritative unresolved set. Enables the
   *  attention-first sort and the per-row dismiss. */
  pendingDeliverableIds?: string[]
  /** Called after a successful dismiss so the owner re-pulls the conversation projection
   *  (counts, pending ids, the derived board column). The list refetches its
   *  own rows regardless. */
  onResolved?: () => void
  /** Soft-refetch trigger — pass artifactSetKey(conversation) so the rows stay current
   *  as the set changes shape. Unlike a conversationId change, this keeps the stale rows
   *  in place until the new set lands (no loading flash). */
  refreshKey?: string
}

// The largest page the route serves. The approval rows sort to the top of what
// is loaded, so the bigger the first page the less likely a draft PR or a ready
// review sits behind "Load more".
const PAGE_REQUEST = { page_size: 200 }

export default function DeliverableList({
  conversationId,
  onOpenApproval,
  pendingDeliverableIds,
  onResolved,
  refreshKey,
}: Props) {
  // The hook is keyed by path and the path carries the conversation id, so a
  // conversation change swaps its target, and a page token minted for one
  // conversation is never sent against another.
  const list = usePagedList<Deliverable>(
    `/api/agent/conversations/${conversationId}/deliverables/list`,
    "Couldn't load this run's deliverables.",
  )
  const { items: deliverables, total, hasMore, loading, error, load, loadMore, setItems } = list
  const [loaded, setLoaded] = useState(false)
  // Per-id in-flight guard so a row's [x] disables only itself while its POST
  // is in flight (and rapid double-clicks can't fire two dismisses).
  const [dismissing, setDismissing] = useState<Record<string, boolean>>({})
  // Which conversation the rows on screen belong to.
  const shownConversation = useRef('')

  // A conversation change resets to the loading state; a refreshKey change must
  // not — it re-pulls behind the rows already on screen. The reset runs before
  // the falsy-conversationId skip, so the old conversation's rows are gone even
  // when no new load follows, and the effect cleanup keeps a response still in
  // flight for the old conversation from marking the list loaded.
  useEffect(() => {
    let cancelled = false
    if (shownConversation.current !== conversationId) {
      shownConversation.current = conversationId
      // The clear must land before the new conversation's rows do, so it is
      // synchronous by design.
      setLoaded(false)
      setItems(() => [])
    }
    if (!conversationId) return
    // Only a page that actually arrived flips `loaded`, so a failed first load
    // falls to the error branch rather than to the empty state.
    load(PAGE_REQUEST).then((page) => {
      if (page && !cancelled) setLoaded(true)
    })
    return () => {
      cancelled = true
    }
  }, [conversationId, refreshKey, load, setItems])

  const pendingSet = useMemo(() => new Set(pendingDeliverableIds ?? []), [pendingDeliverableIds])

  // Attention-first: unresolved rows in the projection's order, then everything
  // else in fetch order (sort is stable, so ties keep it).
  const ordered = useMemo(() => {
    if (pendingSet.size === 0) return deliverables
    const rank = new Map((pendingDeliverableIds ?? []).map((id, i) => [id, i]))
    return [...deliverables].sort(
      (a, b) =>
        (rank.get(a.id) ?? Number.MAX_SAFE_INTEGER) - (rank.get(b.id) ?? Number.MAX_SAFE_INTEGER),
    )
  }, [deliverables, pendingDeliverableIds, pendingSet])

  const dismissOne = async (a: Deliverable) => {
    if (dismissing[a.id]) return
    setDismissing((m) => ({ ...m, [a.id]: true }))
    try {
      await dismissDeliverable(a.id, a.kind)
      // Resolution is async + non-blocking: it touches only the GitHub object +
      // deliverable row, never the conversation. Reload in place (the row flips to its
      // resolved state) and let the owner re-derive the projection.
      await load(PAGE_REQUEST)
      onResolved?.()
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not dismiss the deliverable.'))
    } finally {
      setDismissing((m) => {
        const next = { ...m }
        delete next[a.id]
        return next
      })
    }
  }

  // A failed load stands in for the rows only when there are none to show (the
  // initial load, or a conversation change). A failed SOFT refetch keeps the rows it
  // has — they were true a moment ago, and blanking an always-mounted list on
  // a transient error defeats the point of refreshKey — and the next
  // set-shape change retries.
  // The hook keeps its last error until a load succeeds, so a load in flight
  // (the next conversation's) shows as loading rather than as that error.
  if (error && !loaded && !loading) {
    return <p className="text-secondary leading-relaxed text-alarm">{error}</p>
  }
  if (!loaded) {
    return <p className="text-secondary text-ink-3/70">Loading deliverables…</p>
  }
  if (ordered.length === 0) {
    return <p className="text-secondary text-ink-3/70">No deliverables yet.</p>
  }

  return (
    <>
      <ul className="space-y-1.5">
        {ordered.map((a) => (
          <DeliverableRow
            key={a.id}
            deliverable={a}
            onOpenApproval={onOpenApproval}
            onDismiss={pendingSet.has(a.id) ? () => void dismissOne(a) : undefined}
            dismissing={!!dismissing[a.id]}
          />
        ))}
      </ul>
      {hasMore && (
        <button
          type="button"
          onClick={loadMore}
          disabled={loading}
          className="mt-1.5 text-label text-warm transition-colors hover:text-warm/70 disabled:opacity-50"
        >
          {loading
            ? 'Loading…'
            : `Load more (${deliverables.length} of ${total ?? deliverables.length})`}
        </button>
      )}
    </>
  )
}
