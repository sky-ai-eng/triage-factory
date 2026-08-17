import { useEffect, useRef, useState } from 'react'
import type { ActivityAction } from '../types'
import { usePagedList } from '../hooks/usePagedList'
import { ActionRow } from './ActionRow'

// ActionList — every external write one run performed, newest first. The
// sibling of ArtifactList, and the other half of "what did this run do".
//
// The two lists answer different questions and neither subsumes the other.
// Artifacts are the OBJECTS a run owns and the state they are in now — a PR, a
// review, a branch — which is what an approval surface needs. Actions are the
// WRITES it made, including every one that leaves no object behind: a reply on
// a review thread, a merge the upstream refused, a push the gate turned down, a
// host the sandbox blocked. A run whose only external act was a reply produces
// no artifact at all, and before this list that run's record read as empty.
//
// Reads POST /api/agent/conversations/{id}/actions/list, which is team-scoped
// through the run. It used to answer a hardcoded 200 newest rows and admit so
// in a footnote; now it pages, and "older actions are in the activity feed"
// becomes a "load more" that actually fetches them.
interface Props {
  conversationId: string
  /** Soft-refetch trigger. Unlike a conversationId change this keeps the rows already on
   *  screen until the new set lands, so a live run's list doesn't blink. */
  refreshKey?: string
}

export default function ActionList({ conversationId, refreshKey }: Props) {
  // The hook is keyed by path and the path carries the run id, so a run change
  // swaps its target — which is also what keeps a page token minted for one
  // run from ever being sent against another.
  const list = usePagedList<ActivityAction>(
    `/api/agent/conversations/${conversationId}/actions/list`,
    "Couldn't load this run's actions.",
  )
  const { items: actions, total, hasMore, loading, error, load, loadMore, setItems } = list
  const [loaded, setLoaded] = useState(false)
  // Which run the rows on screen belong to. A run change clears them before the
  // new run's land, so one run's actions never render under another's heading;
  // a soft refetch (refreshKey) deliberately does not clear, because those rows
  // were true a moment ago and this is an audit surface.
  const shownRun = useRef('')

  useEffect(() => {
    if (!conversationId) return
    let cancelled = false
    if (shownRun.current !== conversationId) {
      shownRun.current = conversationId
      // The clear must land before the new run's rows do, so it is synchronous
      // by design — the alternative renders one run's actions under another
      // run's heading for a frame.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setLoaded(false)
      setItems(() => [])
    }
    // Only a page that actually arrived flips `loaded` — a failed FIRST load
    // must fall to the error branch, not to "no external actions yet", which
    // would be a false statement about an audit surface. A failed refetch
    // leaves `loaded` already true, which is what keeps the stale rows up.
    load({}).then((page) => {
      if (page && !cancelled) setLoaded(true)
    })
    return () => {
      cancelled = true
    }
  }, [conversationId, refreshKey, load, setItems])

  // A failed load stands in for the rows only when there are none to show. A
  // failed soft refetch keeps what it has — those rows were true a moment ago,
  // and this is an audit surface, so blanking it on a transient error is worse
  // than showing a slightly stale copy.
  if (error && !loaded) {
    return <p className="text-[11.5px] leading-relaxed text-dismiss">{error}</p>
  }
  if (!loaded) {
    return <p className="text-[11.5px] text-text-tertiary/70">Loading actions…</p>
  }
  if (actions.length === 0) {
    return <p className="text-[11.5px] text-text-tertiary/70">No external actions yet.</p>
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
          className="mt-1.5 text-[10px] text-accent transition-colors hover:text-accent/70 disabled:opacity-50"
        >
          {loading ? 'Loading…' : `Load more (${actions.length} of ${total ?? actions.length})`}
        </button>
      )}
    </>
  )
}
