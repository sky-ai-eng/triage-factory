import { useEffect, useRef, useState } from 'react'
import { Archive, AlertTriangle } from 'lucide-react'
import { archiveTeam, fetchArchivePreview, type ArchivePreview } from '../lib/teamLifecycle'
import { useFocusTrap } from '../hooks/useFocusTrap'

interface ArchiveTeamModalProps {
  teamId: string
  teamName: string
  // onDone fires after a successful archive (parent refreshes the team list +
  // toasts the counts). onClose is cancel / backdrop / Escape.
  onDone: (cancelledRuns: number, cancelledCuratorSessions: number) => void
  onClose: () => void
}

// ArchiveTeamModal is the org-admin destructive confirm for archiving a team
// (TFAC-448). It loads the live-work counts up front so the warning is concrete
// ("ends N delegations and M curator sessions now"), then performs a single
// destructive confirm — there is no "let it finish" branch by design. Styling
// mirrors TransferOwnershipModal (the shared destructive-modal shell).
export default function ArchiveTeamModal({
  teamId,
  teamName,
  onDone,
  onClose,
}: ArchiveTeamModalProps) {
  const [preview, setPreview] = useState<ArchivePreview | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2). Initial focus lands on Cancel — the non-destructive
  // default for a destructive confirm.
  const dialogRef = useRef<HTMLDivElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  useFocusTrap(dialogRef, { initialFocus: cancelRef })

  useEffect(() => {
    let cancelled = false
    fetchArchivePreview(teamId)
      .then((p) => {
        if (!cancelled) setPreview(p)
      })
      .catch((e) => {
        if (!cancelled) setLoadError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      cancelled = true
    }
  }, [teamId])

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
      onDone(res.cancelled_runs, res.cancelled_curator_sessions)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not archive the team.')
    } finally {
      setSubmitting(false)
    }
  }

  const runs = preview?.active_runs ?? 0
  const sessions = preview?.active_curator_sessions ?? 0

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={() => {
        if (!submitting) onClose()
      }}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        className="flex max-h-[85vh] w-full max-w-md flex-col overflow-hidden rounded-2xl border border-border-glass bg-surface-raised shadow-lg shadow-black/[0.04] backdrop-blur-xl"
        role="dialog"
        aria-modal="true"
        aria-labelledby="archive-team-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 pb-4 pt-6">
          <div className="flex items-center gap-2.5">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-dismiss/10 text-dismiss">
              <Archive size={15} />
            </span>
            <h2
              id="archive-team-title"
              className="text-[18px] font-semibold tracking-tight text-text-primary"
            >
              Archive {teamName}
            </h2>
          </div>

          <p className="mt-3 text-[13px] leading-relaxed text-text-secondary">
            {loadError ? (
              <span className="text-dismiss">{loadError}</span>
            ) : preview === null ? (
              'Checking for in-flight work…'
            ) : (
              <>
                Archiving ends{' '}
                <strong className="font-semibold text-text-primary">
                  {runs} active {runs === 1 ? 'delegation' : 'delegations'}
                </strong>{' '}
                and{' '}
                <strong className="font-semibold text-text-primary">
                  {sessions} curator {sessions === 1 ? 'session' : 'sessions'}
                </strong>{' '}
                now. The team disappears for everyone and all writes are blocked.
              </>
            )}
          </p>

          <div className="mt-3 flex items-start gap-2 rounded-xl border border-dismiss/20 bg-dismiss/[0.04] px-3 py-2.5">
            <AlertTriangle size={14} className="mt-0.5 shrink-0 text-dismiss" />
            <p className="text-[12px] leading-relaxed text-text-tertiary">
              This can&rsquo;t be resumed — restoring the team later does not bring its stopped runs
              back. An org admin can restore the team from the Teams settings.
            </p>
          </div>
        </div>

        {error && <p className="px-6 pt-1 text-[12px] text-dismiss">{error}</p>}

        <div className="flex items-center justify-end gap-3 border-t border-border-subtle px-6 py-4">
          <button
            ref={cancelRef}
            type="button"
            onClick={onClose}
            disabled={submitting}
            className="text-[12px] text-text-tertiary transition-colors hover:text-text-secondary disabled:opacity-40"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={submit}
            // Stay disabled until the preview resolves so the user can't confirm
            // before seeing the active-work counts (the warning the modal exists for).
            disabled={submitting || loadError !== null || preview === null}
            className="rounded-xl bg-dismiss px-5 py-2 text-[13px] font-medium text-white transition-colors hover:bg-dismiss/90 disabled:opacity-40"
          >
            {submitting ? 'Archiving…' : 'Archive team'}
          </button>
        </div>
      </div>
    </div>
  )
}
