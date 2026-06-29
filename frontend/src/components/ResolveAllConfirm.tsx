import { useEffect, useId, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { resolveAllSummary } from '../lib/approval'

interface Props {
  open: boolean
  /** Unresolved draft-PR count, from the run projection's unresolved-artifact
   *  derivation (the same set the backend teardown force-resolves). */
  prCount: number
  /** Unresolved ready-review count, same source. */
  reviewCount: number
  /** True when the run is still executing — the teardown also cancels it, so the
   *  copy adds a "the run will be cancelled" note. */
  isLive: boolean
  /** The gesture's verb, e.g. "Return to queue" / "Mark done" — the confirm
   *  button label, so the dialog reads as the action it gates. */
  actionLabel: string
  /** Disable the buttons while the underlying request is in flight. */
  busy?: boolean
  onConfirm: () => void
  onCancel: () => void
}

// ResolveAllConfirm is the second gesture's gate (TFAC-384 §4): dragging a task
// to Done or back to the queue *force-resolves every unresolved artifact* (the
// generalized teardown) and, if the run is live, cancels it. Because that closes
// real GitHub draft PRs and discards pending reviews, it's destructive enough to
// confirm — parameterized by the actual set ("Close 2 draft PRs and discard 1
// pending review? Pushed branches are kept."). Pure + instant: the counts come
// from the caller's run projection, so there's no fetch/loading state in the
// dialog. The per-item resolve (approve/dismiss one) is the OTHER gesture and is
// never routed through here — do not conflate them.
export default function ResolveAllConfirm({
  open,
  prCount,
  reviewCount,
  isLive,
  actionLabel,
  busy,
  onConfirm,
  onCancel,
}: Props) {
  const titleId = useId()
  const descId = useId()
  const dialogRef = useRef<HTMLDivElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)

  // Trap focus inside the dialog while open and restore it on close (WCAG
  // 2.1.2). Initial focus lands on Cancel (the non-destructive default), so a
  // keyboard user doesn't fire the destructive confirm by reflex on first Enter.
  useFocusTrap(dialogRef, { active: open, initialFocus: cancelRef })

  // Esc cancels — but never while the request is in flight (a mid-teardown
  // dismiss would desync the UI from the half-applied backend state).
  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onCancel()
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [open, busy, onCancel])

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            className="fixed inset-0 z-[60] bg-black/30 backdrop-blur-sm"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={() => !busy && onCancel()}
          />
          <motion.div
            ref={dialogRef}
            tabIndex={-1}
            role="alertdialog"
            aria-modal="true"
            aria-labelledby={titleId}
            aria-describedby={descId}
            className="fixed left-1/2 top-1/2 z-[60] w-[min(440px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border-glass bg-surface/95 p-5 shadow-2xl shadow-black/[0.12] backdrop-blur-2xl focus:outline-none"
            initial={{ opacity: 0, scale: 0.96, y: 8 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.96, y: 8 }}
            transition={{ type: 'spring', damping: 28, stiffness: 360 }}
            onClick={(e) => e.stopPropagation()}
          >
            <h2 id={titleId} className="text-[14px] font-semibold tracking-tight text-text-primary">
              Resolve open artifacts?
            </h2>
            <p id={descId} className="mt-2 text-[12.5px] leading-relaxed text-text-secondary">
              {resolveAllSummary(prCount, reviewCount, isLive)}
            </p>
            <div className="mt-5 flex items-center justify-end gap-2">
              <button
                ref={cancelRef}
                type="button"
                onClick={onCancel}
                disabled={busy}
                className="rounded-[5px] px-3 py-1.5 text-[12px] font-medium text-text-tertiary transition-colors hover:bg-black/[0.04] hover:text-text-secondary disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={onConfirm}
                disabled={busy}
                className="rounded-[5px] bg-dismiss px-3 py-1.5 text-[12px] font-semibold text-white transition-opacity hover:opacity-90 disabled:cursor-wait disabled:opacity-60"
              >
                {busy ? 'Working…' : actionLabel}
              </button>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
