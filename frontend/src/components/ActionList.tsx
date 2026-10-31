import { useEffect, useRef, useState } from 'react'
import type { ActivityAction } from '../types'
import { usePagedList } from '../hooks/usePagedList'
import { ActionRow } from './ActionRow'

// ActionList — the external writes one conversation performed, newest first,
// narrowed to the writes on one object (`objectId`, an artifact's object_id) or
// to the refused and failed ones (`refused`). The Run page's Artifacts section
// expands into it: an artifact row lists the actions behind it, and the
// "refused or failed" line lists the writes that changed nothing.
//
// Reads POST /api/agent/conversations/{id}/actions/list, which is team-scoped
// through the conversation, a page at a time with "load more" for the rest.
interface Props {
  conversationId: string
  /** Keep only the actions on this object (an artifact's object_id). */
  objectId?: string
  /** Keep only the refused and failed writes. */
  refused?: boolean
  /** Soft-refetch trigger. Unlike a conversationId change this keeps the rows already on
   *  screen until the new set lands, so a live conversation's list doesn't blink. */
  refreshKey?: string
}

export default function ActionList({ conversationId, objectId, refused, refreshKey }: Props) {
  // The hook is keyed by path and the path carries the conversation id, so a conversation change
  // swaps its target — which is also what keeps a page token minted for one
  // conversation from ever being sent against another.
  const list = usePagedList<ActivityAction>(
    `/api/agent/conversations/${conversationId}/actions/list`,
    "Couldn't load this run's actions.",
  )
  const { items: actions, total, hasMore, loading, error, load, loadMore, setItems } = list
  const [loaded, setLoaded] = useState(false)
  // Which read the rows on screen belong to: the conversation and the filters.
  // A change of either clears them before the new read's land, so one
  // conversation's actions never render under another's heading; a soft
  // refetch (refreshKey) deliberately does not clear, because those rows were
  // true a moment ago and this is an audit surface.
  const shownRead = useRef('')

  useEffect(() => {
    if (!conversationId) return
    let cancelled = false
    const read = `${conversationId}|${objectId ?? ''}|${refused ? 'refused' : ''}`
    if (shownRead.current !== read) {
      shownRead.current = read
      // The clear must land before the new read's rows do, so it is synchronous
      // by design — the alternative renders one read's actions under another's
      // heading for a frame.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setLoaded(false)
      setItems(() => [])
    }
    // Only a page that actually arrived flips `loaded` — a failed FIRST load
    // must fall to the error branch, not to "no external actions yet", which
    // would be a false statement about an audit surface. A failed refetch
    // leaves `loaded` already true, which is what keeps the stale rows up.
    load({
      ...(objectId ? { object_id: objectId } : {}),
      ...(refused ? { refused: true } : {}),
    }).then((page) => {
      if (page && !cancelled) setLoaded(true)
    })
    return () => {
      cancelled = true
    }
  }, [conversationId, objectId, refused, refreshKey, load, setItems])

  // A failed load stands in for the rows only when there are none to show. A
  // failed soft refetch keeps what it has — those rows were true a moment ago,
  // and this is an audit surface, so blanking it on a transient error is worse
  // than showing a slightly stale copy.
  if (error && !loaded) {
    return <p className="text-secondary leading-relaxed text-alarm">{error}</p>
  }
  if (!loaded) {
    return <p className="text-secondary text-ink-3/70">Loading actions…</p>
  }
  if (actions.length === 0) {
    return <p className="text-secondary text-ink-3/70">No external actions yet.</p>
  }

  return (
    <>
      <ul className="space-y-1.5">
        {actions.map((a) => (
          <ActionRow key={a.id} action={a} />
        ))}
      </ul>
      {hasMore && (
        <button
          type="button"
          onClick={loadMore}
          disabled={loading}
          className="mt-1.5 text-label text-warm transition-colors hover:text-warm/70 disabled:opacity-50"
        >
          {loading ? 'Loading…' : `Load more (${actions.length} of ${total ?? actions.length})`}
        </button>
      )}
    </>
  )
}
