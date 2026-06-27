import { useEffect, useState } from 'react'
import type { Artifact } from '../types'
import { readError } from '../lib/api'
import { ArtifactRow } from './ArtifactRow'

// ArtifactList — the shared surface for everything a run produced (TFAC-470).
// One row per artifact: a kind icon + the target + a state badge + an external
// link. Fetched run-scoped from GET /api/agent/runs/{id}/artifacts (TFAC-465);
// the component owns its own fetch so both consumers stay one-liners — the
// run-detail rail (mounts with the page → "always visible") and the board
// card's popover (mounts on open → lazy-fetch).
//
// The row rendering itself lives in ArtifactRow.tsx, shared with the
// bot-activity audit feed (TFAC-483). pull_request / review rows open their
// approval overlays (PendingPROverlay / ReviewOverlay, TFAC-462/463) by artifact
// id via onOpenApproval — those overlays are owned by the parent (Board /
// RunDetail), so this list never duplicates them. Every other kind (branch /
// issue / comment), and any row when no handler is wired, just links out to
// `url`.
interface Props {
  runId: string
  onOpenApproval?: (kind: 'review' | 'pr', artifactId: string) => void
}

export default function ArtifactList({ runId, onOpenApproval }: Props) {
  const [artifacts, setArtifacts] = useState<Artifact[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!runId) return
    let cancelled = false
    setArtifacts(null)
    setError(null)
    ;(async () => {
      try {
        const res = await fetch(`/api/agent/runs/${runId}/artifacts`)
        if (!res.ok) {
          // readError keeps the context ("Couldn't load artifacts: …") and
          // degrades through the server's JSON `error`, a non-JSON text body,
          // then the bare status code — never a context-less raw message.
          throw new Error(await readError(res, "Couldn't load artifacts"))
        }
        const data = (await res.json()) as Artifact[]
        if (!cancelled) setArtifacts(data ?? [])
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [runId])

  if (error) {
    return <p className="text-[11.5px] leading-relaxed text-dismiss">{error}</p>
  }
  if (artifacts === null) {
    return <p className="text-[11.5px] text-text-tertiary/70">Loading artifacts…</p>
  }
  if (artifacts.length === 0) {
    return <p className="text-[11.5px] text-text-tertiary/70">No artifacts yet.</p>
  }

  return (
    <ul className="space-y-1.5">
      {artifacts.map((a) => (
        <ArtifactRow key={a.id} artifact={a} onOpenApproval={onOpenApproval} />
      ))}
    </ul>
  )
}
