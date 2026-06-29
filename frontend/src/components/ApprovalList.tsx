import { useEffect, useState } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { ExternalLink, X } from 'lucide-react'
import type { Artifact } from '../types'
import { kindForArtifact } from '../lib/approval'
import { metaForKind } from './artifactMeta'
import { StateBadge } from './ArtifactRow'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'

interface Props {
  open: boolean
  onClose: () => void
  /** The unresolved approvable subset (draft PRs + ready reviews), in the
   *  backend's order. One card per item — each independently editable/approvable
   *  via its overlay and dismissable in place. */
  items: Artifact[]
  /** Open one item's editor/approval overlay (PendingPROverlay / ReviewOverlay)
   *  by artifact id — owned by the parent so the list never duplicates them. */
  onOpenItem: (kind: 'review' | 'pr', artifactId: string) => void
  /** Called after a successful in-place dismiss so the parent re-pulls the run +
   *  artifact set (the resolved card drops out, and the last resolution
   *  re-derives the run's column). */
  onDismissed: () => void
}

// ApprovalList is the artifact-sidecar approval roster (TFAC-384 §1/§2): the set
// of a run's unresolved draft PRs + ready reviews as a *list* of cards, replacing
// the old single-overlay singleton. Each row opens its own per-item editor by
// artifact id (the overlays already key off one) and carries an [x] dismiss —
// shown even with a single item. Resolving one (approve or dismiss) leaves the
// others; the list empties as items resolve, and the parent closes it once the
// last is gone (no "mark done vs keep open" prompt — the task auto-resolves from
// the derived state). The full mixed-kind dock is TFAC-385; this is the minimal
// list.
export default function ApprovalList({ open, onClose, items, onOpenItem, onDismissed }: Props) {
  // Per-id in-flight guard so a card's [x] disables only itself while its POST
  // is in flight (and rapid double-clicks can't fire two dismisses).
  const [dismissing, setDismissing] = useState<Record<string, boolean>>({})

  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [open, onClose])

  const dismissOne = async (a: Artifact) => {
    if (dismissing[a.id]) return
    setDismissing((m) => ({ ...m, [a.id]: true }))
    try {
      const res = await fetch(`/api/artifacts/${a.id}/dismiss`, { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to dismiss'))
        return
      }
      // Resolution is async + non-blocking: it touches only the GitHub object +
      // artifact row, never the run. Re-pull so the card drops out of the set.
      onDismissed()
    } catch (err) {
      toast.error(`Failed to dismiss: ${(err as Error).message}`)
    } finally {
      setDismissing((m) => {
        const next = { ...m }
        delete next[a.id]
        return next
      })
    }
  }

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            className="fixed inset-0 z-40 bg-black/20 backdrop-blur-sm"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />
          <motion.div
            className="fixed inset-x-0 bottom-0 top-auto z-40 mx-auto flex max-h-[80vh] w-[min(680px,calc(100vw-1.5rem))] flex-col rounded-t-3xl border border-border-glass bg-surface/95 shadow-2xl shadow-black/[0.1] backdrop-blur-2xl sm:inset-y-12 sm:bottom-auto sm:top-1/2 sm:max-h-[76vh] sm:-translate-y-1/2 sm:rounded-3xl"
            initial={{ opacity: 0, y: 24 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 24 }}
            transition={{ type: 'spring', damping: 30, stiffness: 340 }}
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex shrink-0 items-center justify-between border-b border-border-subtle px-5 py-3.5">
              <div className="flex items-center gap-2.5">
                <span className="h-2 w-2 rounded-full bg-snooze" />
                <h2 className="text-[14px] font-semibold tracking-tight text-text-primary">
                  Awaiting your approval
                </h2>
                <span className="font-mono text-[11px] tabular-nums text-text-tertiary">
                  {items.length} {items.length === 1 ? 'item' : 'items'}
                </span>
              </div>
              <button
                type="button"
                onClick={onClose}
                aria-label="Close"
                className="rounded-lg px-2 py-1 text-lg leading-none text-text-tertiary transition-colors hover:bg-black/[0.03] hover:text-text-secondary"
              >
                &times;
              </button>
            </div>

            <div className="min-h-0 flex-1 overflow-y-auto p-4">
              {items.length === 0 ? (
                <p className="py-12 text-center text-[12.5px] text-text-tertiary">
                  All artifacts resolved.
                </p>
              ) : (
                <ul className="space-y-2.5">
                  {items.map((a) => (
                    <ApprovalCard
                      key={a.id}
                      artifact={a}
                      dismissing={!!dismissing[a.id]}
                      onOpen={onOpenItem}
                      onDismiss={() => dismissOne(a)}
                    />
                  ))}
                </ul>
              )}
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}

// ApprovalCard — one unresolved artifact: kind icon + target + state, an
// "Open & edit" action that raises the per-item overlay (edit title/body or
// inline comments, then approve), and an [x] dismiss. The external-link affordance
// stays reachable so the user can jump to the live object on GitHub.
function ApprovalCard({
  artifact,
  dismissing,
  onOpen,
  onDismiss,
}: {
  artifact: Artifact
  dismissing: boolean
  onOpen: (kind: 'review' | 'pr', artifactId: string) => void
  onDismiss: () => void
}) {
  const meta = metaForKind(artifact.kind)
  const Icon = meta.icon
  const kind = kindForArtifact(artifact)
  const label = kind === 'pr' ? 'Open & edit' : 'Review & edit'

  return (
    <li className="flex items-center gap-3 rounded-xl border border-border-subtle bg-black/[0.015] px-3.5 py-3">
      <Icon size={16} className={`shrink-0 ${meta.text}`} aria-hidden />
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate font-mono text-[12px] text-text-primary">
          {artifact.target || artifact.external_id || meta.label}
        </span>
        <span className="flex items-center gap-2">
          <span className="font-mono text-[10px] uppercase tracking-[0.1em] text-text-tertiary/70">
            {meta.label}
          </span>
          <StateBadge state={artifact.state} />
        </span>
      </div>

      {artifact.url && (
        <a
          href={artifact.url}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={`Open ${meta.label} on its source (new tab)`}
          title="Open source (new tab)"
          className="inline-flex shrink-0 items-center rounded-[5px] p-1.5 text-text-tertiary/70 transition-colors hover:bg-black/[0.04] hover:text-text-secondary"
        >
          <ExternalLink size={13} aria-hidden />
        </a>
      )}

      {kind && (
        <button
          type="button"
          onClick={() => onOpen(kind, artifact.id)}
          className="shrink-0 rounded-[5px] bg-snooze/[0.12] px-2.5 py-1.5 text-[11px] font-semibold text-snooze shadow-[inset_0_0_0_1px_color-mix(in_srgb,var(--color-snooze)_26%,transparent)] transition-colors hover:bg-snooze/[0.2]"
        >
          {label} →
        </button>
      )}

      <button
        type="button"
        onClick={onDismiss}
        disabled={dismissing}
        aria-label="Dismiss"
        title="Dismiss — closes the draft PR / discards the pending review (branch kept)"
        className="inline-flex shrink-0 items-center justify-center rounded-[5px] p-1.5 text-text-tertiary/70 transition-colors hover:bg-dismiss/[0.1] hover:text-dismiss disabled:cursor-wait disabled:opacity-50"
      >
        <X size={14} aria-hidden />
      </button>
    </li>
  )
}
