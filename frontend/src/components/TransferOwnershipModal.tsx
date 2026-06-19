import { useEffect, useState } from 'react'
import { Crown } from 'lucide-react'
import { messageFrom, type RosterMember } from '../hooks/useMemberRoster'

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
// admin. Styling mirrors ProjectBackfillModal (the shared modal shell).
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
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={() => {
        if (!submitting) onClose()
      }}
    >
      <div
        className="flex max-h-[85vh] w-full max-w-md flex-col overflow-hidden rounded-2xl border border-border-glass bg-surface-raised shadow-lg shadow-black/[0.04] backdrop-blur-xl"
        role="dialog"
        aria-modal="true"
        aria-labelledby="transfer-ownership-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 pb-4 pt-6">
          <div className="flex items-center gap-2.5">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent-soft text-accent">
              <Crown size={15} />
            </span>
            <h2
              id="transfer-ownership-title"
              className="text-[18px] font-semibold tracking-tight text-text-primary"
            >
              Transfer ownership
            </h2>
          </div>
          <p className="mt-2 text-[13px] leading-relaxed text-text-tertiary">
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
                    selected === m.userId ? 'bg-accent-soft' : 'hover:bg-black/[0.02]'
                  }`}
                >
                  <input
                    type="radio"
                    name="transfer-owner-candidate"
                    value={m.userId}
                    checked={selected === m.userId}
                    onChange={() => setSelected(m.userId)}
                    disabled={submitting}
                    className="h-4 w-4 accent-accent"
                  />
                  <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-accent-soft text-[12px] font-semibold text-accent">
                    {(m.displayName.trim()[0] ?? '?').toUpperCase()}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-[13px] font-medium text-text-primary">
                      {m.displayName || 'Unnamed user'}
                    </div>
                    <div className="text-[11px] capitalize text-text-tertiary">{m.role}</div>
                  </div>
                </label>
              </li>
            ))}
          </ul>
        </div>

        {error && <p className="px-6 pt-1 text-[12px] text-dismiss">{error}</p>}

        <div className="flex items-center justify-end gap-3 border-t border-border-subtle px-6 py-4">
          <button
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
            disabled={!selected || submitting}
            className="rounded-xl bg-accent px-5 py-2 text-[13px] font-medium text-white transition-colors hover:bg-accent/90 disabled:opacity-40"
          >
            {submitting ? 'Transferring…' : 'Transfer ownership'}
          </button>
        </div>
      </div>
    </div>
  )
}
