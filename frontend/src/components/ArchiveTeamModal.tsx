import { useEffect, useRef, useState } from 'react'
import { Archive, AlertTriangle } from 'lucide-react'
import { archiveTeam, type ArchivePreview } from '../lib/teamLifecycle'
import { useFocusTrap } from '../hooks/useFocusTrap'

interface ArchiveTeamModalProps {
  teamId: string
  teamName: string
  // The live-work counts, fetched by the opener BEFORE the dialog opens: a
  // destructive confirm that cannot state its consequences is withdrawn, not
  // opened empty, so the counts are a prop rather than a fetch in here.
  preview: ArchivePreview
  // onDone fires after a successful archive (parent refreshes the team list +
  // toasts the count). onClose is cancel / backdrop / Escape.
  onDone: (cancelledRuns: number) => void
  onClose: () => void
}

// ArchiveTeamModal is the org-admin destructive confirm for archiving a team.
// It opens already knowing the live-work count, so the warning is concrete
// ("ends N delegations now") from its first frame, then performs a single
// destructive confirm — there is no "let it finish" branch by design.
// Styling mirrors TransferOwnershipModal (the shared destructive-modal shell).
export default function ArchiveTeamModal({
  teamId,
  teamName,
  preview,
  onDone,
  onClose,
}: ArchiveTeamModalProps) {
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2). Initial focus lands on Cancel — the non-destructive
  // default for a destructive confirm.
  const dialogRef = useRef<HTMLDivElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  useFocusTrap(dialogRef, { initialFocus: cancelRef })

  // Escape closes unless an archive is in flight.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, submitting])

  const submit = async () => {
    if (submitting) return
    setSubmitting(true)
    setError(null)
    try {
      const res = await archiveTeam(teamId)
      onDone(res.cancelled_runs)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not archive the team.')
    } finally {
      setSubmitting(false)
    }
  }

  const runs = preview.active_runs

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-scrim backdrop-blur-sm"
      onClick={() => {
        if (!submitting) onClose()
      }}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        className="flex max-h-[85vh] w-full max-w-md flex-col overflow-hidden rounded-2xl border border-line-1 bg-raised shadow-float shadow-black/[0.04] backdrop-blur-xl"
        role="dialog"
        aria-modal="true"
        aria-labelledby="archive-team-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 pb-4 pt-6">
          <div className="flex items-center gap-2.5">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-alarm/10 text-alarm">
              <Archive size={15} />
            </span>
            <h2
              id="archive-team-title"
              className="text-section font-semibold tracking-tight text-ink-1"
            >
              Archive {teamName}
            </h2>
          </div>

          <p className="mt-3 text-body leading-relaxed text-ink-2">
            Archiving ends{' '}
            <strong className="font-semibold text-ink-1">
              {runs} active {runs === 1 ? 'delegation' : 'delegations'}
            </strong>{' '}
            now. The team disappears for everyone and all writes are blocked.
          </p>

          <div className="mt-3 flex items-start gap-2 rounded-xl border border-alarm/20 bg-alarm/[0.04] px-3 py-2.5">
            <AlertTriangle size={14} className="mt-0.5 shrink-0 text-alarm" />
            <p className="text-ui leading-relaxed text-ink-3">
              This can&rsquo;t be resumed — restoring the team later does not bring its stopped runs
              back. An org admin can restore the team from the Teams settings.
            </p>
          </div>
        </div>

        {error && <p className="px-6 pt-1 text-ui text-alarm">{error}</p>}

        <div className="flex items-center justify-end gap-3 border-t border-line-1 px-6 py-4">
          <button
            ref={cancelRef}
            type="button"
            onClick={onClose}
            disabled={submitting}
            className="text-ui text-ink-3 transition-colors hover:text-ink-2 disabled:opacity-40"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={submit}
            disabled={submitting}
            className="rounded-xl bg-alarm px-5 py-2 text-body font-medium text-ground transition-colors hover:bg-alarm/90 disabled:opacity-40"
          >
            {submitting ? 'Archiving…' : 'Archive team'}
          </button>
        </div>
      </div>
    </div>
  )
}
