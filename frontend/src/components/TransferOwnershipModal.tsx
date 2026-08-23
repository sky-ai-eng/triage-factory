import { useEffect, useRef, useState } from 'react'
import { Crown } from 'lucide-react'
import { useFocusTrap } from '../hooks/useFocusTrap'
import {
  messageFrom,
  displayNameFor,
  initialFor,
  type RosterMember,
} from '../hooks/useMemberRoster'

interface TransferOwnershipModalProps {
  // Eligible new owners — every member except the current owner. The caller
  // only opens the modal when this is non-empty.
  candidates: RosterMember[]
  // 'manual' = the owner clicked "Transfer ownership"; 'leave' = the sole
  // owner clicked Leave and must hand off first (adds the explanatory note).
  reason: 'manual' | 'leave'
  // transfer performs the POST (adapter.transferOwnership). It throws on the
  // backend's 4xx (403/409/422), surfaced inline via messageFrom.
  transfer: (newOwnerUserId: string) => Promise<void>
  // onDone fires after a successful transfer; the parent reloads the roster
  // and closes the modal. onClose is cancel / backdrop / Escape.
  onDone: () => void
  onClose: () => void
}

// TransferOwnershipModal is the owner-only member picker for handing off org
// ownership (TFAC-420). Pick a member, confirm, and the backend promotes them
// to owner, repoints the founder sentinel, and demotes the former owner to
// admin.
export default function TransferOwnershipModal({
  candidates,
  reason,
  transfer,
  onDone,
  onClose,
}: TransferOwnershipModalProps) {
  const [selected, setSelected] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2). Initial focus lands on Cancel — the non-destructive
  // default.
  const dialogRef = useRef<HTMLDivElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  useFocusTrap(dialogRef, { initialFocus: cancelRef })

  // Escape closes unless a transfer is in flight — mirrors the other modals'
  // contract so a mid-request dismiss can't strand the user.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, submitting])

  const submit = async () => {
    if (!selected || submitting) return
    setSubmitting(true)
    setError(null)
    try {
      await transfer(selected)
      onDone()
    } catch (e) {
      // The 409 ("would leave the org without an owner") and 422 ("must be a
      // member") arrive verbatim from the backend; show them in place.
      setError(messageFrom(e, 'Could not transfer ownership.'))
    } finally {
      setSubmitting(false)
    }
  }

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
        aria-labelledby="transfer-ownership-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 pb-4 pt-6">
          <div className="flex items-center gap-2.5">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-warm-2 text-warm">
              <Crown size={15} />
            </span>
            <h2
              id="transfer-ownership-title"
              className="text-section font-semibold tracking-tight text-ink-1"
            >
              Transfer ownership
            </h2>
          </div>
          <p className="mt-2 text-body leading-relaxed text-ink-3">
            {reason === 'leave'
              ? "You're the org's only owner — hand ownership to another member before you can leave."
              : 'Choose the member who becomes the new owner. You stay in the org as an admin.'}
          </p>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto px-6">
          <ul className="space-y-0.5 py-1">
            {candidates.map((m) => (
              <li key={m.userId}>
                <label
                  className={`flex cursor-pointer items-center gap-3 rounded-xl px-3 py-2.5 transition-colors ${
                    selected === m.userId ? 'bg-warm-2' : 'hover:bg-tint-2'
                  }`}
                >
                  <input
                    type="radio"
                    name="transfer-owner-candidate"
                    value={m.userId}
                    checked={selected === m.userId}
                    onChange={() => setSelected(m.userId)}
                    disabled={submitting}
                    className="h-4 w-4 accent-warm"
                  />
                  <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-warm-2 text-ui font-semibold text-warm">
                    {initialFor(m)}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-body font-medium text-ink-1">
                      {displayNameFor(m)}
                    </div>
                    <div className="text-reported capitalize text-ink-3">{m.role}</div>
                  </div>
                </label>
              </li>
            ))}
          </ul>
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
            disabled={!selected || submitting}
            className="rounded-xl bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40"
          >
            {submitting ? 'Transferring…' : 'Transfer ownership'}
          </button>
        </div>
      </div>
    </div>
  )
}
