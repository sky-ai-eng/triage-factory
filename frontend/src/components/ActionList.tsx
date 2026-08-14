import { useCallback, useEffect, useRef, useState } from 'react'
import type { ActivityAction } from '../types'
import { readError } from '../lib/api'
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
// Same fetch shape as ArtifactList (own the fetch, so every consumer is a
// one-liner) against GET /api/agent/conversations/{id}/actions, which is
// team-scoped through the run.
interface Props {
  runId: string
  /** Soft-refetch trigger. Unlike a runId change this keeps the rows already on
   *  screen until the new set lands, so a live run's list doesn't blink. */
  refreshKey?: string
}

// PAGE is the server's cap (runActionsLimit). A full page means the run wrote
// more than this and the older rows live in the governance feed — said out loud
// rather than silently truncated, since a list that claims to be the complete
// answer must admit when it isn't.
const PAGE = 200

export default function ActionList({ runId, refreshKey }: Props) {
  const [actions, setActions] = useState<ActivityAction[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  // Generation counter: a run change or a refetch invalidates in-flight loads
  // so a stale response can't overwrite a newer one.
  const generation = useRef(0)

  const load = useCallback(async () => {
    const gen = ++generation.current
    try {
      const res = await fetch(`/api/agent/conversations/${runId}/actions`)
      if (!res.ok) throw new Error(await readError(res, "Couldn't load actions"))
      const data = (await res.json()) as ActivityAction[]
      if (generation.current === gen) {
        setActions(data ?? [])
        setError(null)
      }
    } catch (err) {
      if (generation.current === gen) {
        setError(err instanceof Error ? err.message : String(err))
      }
    }
  }, [runId])

  useEffect(() => {
    generation.current++
    setActions(null)
    setError(null)
  }, [runId])

  useEffect(() => {
    if (!runId) return
    void load()
  }, [runId, refreshKey, load])

  // A failed load stands in for the rows only when there are none to show. A
  // failed soft refetch keeps what it has — those rows were true a moment ago,
  // and this is an audit surface, so blanking it on a transient error is worse
  // than showing a slightly stale copy.
  if (error && actions === null) {
    return <p className="text-secondary leading-relaxed text-alarm">{error}</p>
  }
  if (actions === null) {
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
      {actions.length >= PAGE && (
        <p className="mt-1.5 text-label text-ink-3/70">
          Most recent {PAGE} — older actions are in the activity feed.
        </p>
      )}
    </>
  )
}
