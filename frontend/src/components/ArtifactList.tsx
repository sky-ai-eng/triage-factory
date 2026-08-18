import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { Artifact } from '../types'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { toast } from './Toast/toastStore'
import { ArtifactRow } from './ArtifactRow'

// ArtifactList — THE surface for every OBJECT a conversation produced, and (since the
// separate approval roster was folded in here) the approval surface too. One
// row per artifact: a kind icon + the target + a state badge + an external
// link. Fetched conversation-scoped from GET /api/agent/conversations/{id}/artifacts;
// the component owns its own fetch so every consumer stays a one-liner — the
// RunDetail rail (mounts with the page → "always visible"), the board card's
// popover and the RunStation dock's popover (mount on open → lazy-fetch).
//
// When the owner passes the conversation projection's `pendingArtifactIds` (the
// authoritative unresolved set — the ready-review predicate lives server-side,
// so artifact.state alone can't reconstruct it), those rows sort to the top in
// the projection's order (draft PRs first, then ready reviews) and carry an
// in-place [x] dismiss. Everything else keeps the backend's order below them.
//
// It is deliberately not the whole answer to "what did this conversation do" — an
// artifact is a lifecycle object, so a write that creates none (a review-thread
// reply, a refused merge, a denied push) can never appear here. ActionList is
// the other half, and the RunStation mounts both.
//
// The row rendering itself lives in ArtifactRow.tsx, shared with the
// bot-activity audit feed. pull_request / review rows open their approval
// overlays (PendingPROverlay / ReviewOverlay) by artifact id via
// onOpenApproval — those overlays are owned by the parent (Board / RunDetail),
// so this list never duplicates them. Every other kind (branch / issue /
// comment), and any row when no handler is wired, just links out to `url`.
interface Props {
  conversationId: string
  onOpenApproval?: (kind: 'review' | 'pr', artifactId: string) => void
  /** The conversation projection's authoritative unresolved set. Enables the
   *  attention-first sort and the per-row dismiss. */
  pendingArtifactIds?: string[]
  /** Called after a successful dismiss so the owner re-pulls the conversation projection
   *  (counts, pending ids, the derived board column). The list refetches its
   *  own rows regardless. */
  onResolved?: () => void
  /** Soft-refetch trigger — pass artifactSetKey(conversation) so the rows stay current
   *  as the set changes shape. Unlike a conversationId change, this keeps the stale rows
   *  in place until the new set lands (no loading flash). */
  refreshKey?: string
}

export default function ArtifactList({
  conversationId,
  onOpenApproval,
  pendingArtifactIds,
  onResolved,
  refreshKey,
}: Props) {
  const [artifacts, setArtifacts] = useState<Artifact[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  // Per-id in-flight guard so a row's [x] disables only itself while its POST
  // is in flight (and rapid double-clicks can't fire two dismisses).
  const [dismissing, setDismissing] = useState<Record<string, boolean>>({})
  // Generation counter: a conversation change or unmount invalidates in-flight loads,
  // and a dismiss-triggered reload can't be clobbered by a stale response.
  const generation = useRef(0)

  const load = useCallback(async () => {
    const gen = ++generation.current
    try {
      const data = await apiJSON<Artifact[]>(`/api/agent/conversations/${conversationId}/artifacts`)
      if (generation.current === gen) {
        setArtifacts(data ?? [])
        setError(null)
      }
    } catch (err) {
      if (generation.current === gen) {
        setError(httpErrorMessage(err, "Couldn't load this run's artifacts."))
      }
    }
  }, [conversationId])

  // A conversation change resets to the loading state; a refreshKey change (below) must
  // not — it re-pulls behind the rows already on screen. The bump here — not
  // just the one inside load() — is what invalidates the old conversation's in-flight
  // response: the load effect skips a falsy conversationId, so without it a stale
  // response could land after this reset and pass the guard.
  useEffect(() => {
    generation.current++
    setArtifacts(null)
    setError(null)
  }, [conversationId])

  // No unmount cleanup needed: each load() bumps the generation on entry, so a
  // newer load invalidates any in-flight predecessor, and a post-unmount
  // setState is a no-op.
  useEffect(() => {
    if (!conversationId) return
    void load()
  }, [conversationId, refreshKey, load])

  const pendingSet = useMemo(() => new Set(pendingArtifactIds ?? []), [pendingArtifactIds])

  // Attention-first: unresolved rows in the projection's order, then everything
  // else in fetch order (sort is stable, so ties keep it).
  const ordered = useMemo(() => {
    if (!artifacts || pendingSet.size === 0) return artifacts
    const rank = new Map((pendingArtifactIds ?? []).map((id, i) => [id, i]))
    return [...artifacts].sort(
      (a, b) =>
        (rank.get(a.id) ?? Number.MAX_SAFE_INTEGER) - (rank.get(b.id) ?? Number.MAX_SAFE_INTEGER),
    )
  }, [artifacts, pendingArtifactIds, pendingSet])

  const dismissOne = async (a: Artifact) => {
    if (dismissing[a.id]) return
    setDismissing((m) => ({ ...m, [a.id]: true }))
    try {
      await apiFetch(`/api/artifacts/${a.id}/dismiss`, { method: 'POST' })
      // Resolution is async + non-blocking: it touches only the GitHub object +
      // artifact row, never the conversation. Reload in place (the row flips to its
      // resolved state) and let the owner re-derive the projection.
      await load()
      onResolved?.()
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not dismiss the artifact.'))
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
  if (error && artifacts === null) {
    return <p className="text-[11.5px] leading-relaxed text-dismiss">{error}</p>
  }
  if (ordered === null) {
    return <p className="text-[11.5px] text-text-tertiary/70">Loading artifacts…</p>
  }
  if (ordered.length === 0) {
    return <p className="text-[11.5px] text-text-tertiary/70">No artifacts yet.</p>
  }

  return (
    <ul className="space-y-1.5">
      {ordered.map((a) => (
        <ArtifactRow
          key={a.id}
          artifact={a}
          onOpenApproval={onOpenApproval}
          onDismiss={pendingSet.has(a.id) ? () => void dismissOne(a) : undefined}
          dismissing={!!dismissing[a.id]}
        />
      ))}
    </ul>
  )
}
