import { Mail, RotateCw, X } from 'lucide-react'
import { CopyLink } from './InviteModal'
import { timeAgo } from '../lib/relativeTime'
import type { PendingInvite } from '../types'

interface Props {
  invite: PendingInvite
  // The fresh accept link from a just-completed resend on THIS invite, if any —
  // revealed inline with a Copy button so the admin can hand off the rotated
  // link. Keyed by the new invite id in the parent.
  freshLink?: string
  // True while a revoke/resend on this row is in flight — disables both
  // controls so a second action can't race the first's reload.
  busy: boolean
  onRevoke: () => void
  onResend: () => void
}

// PendingInviteRow is the org-adapter-specific "ghost" row the People roster
// renders beneath real members (via MemberRoster's extraRows slot). Muted
// styling + a "Pending" badge mark it as not-yet-a-member; Resend rotates the
// token, Revoke kills it. It's an <li> so it slots into the roster's <ul>.
export default function PendingInviteRow({ invite, freshLink, busy, onRevoke, onResend }: Props) {
  return (
    <li className="px-4 py-3 opacity-80">
      <div className="flex items-center gap-4">
        <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border border-dashed border-border-glass bg-black/[0.02] text-text-tertiary">
          <Mail size={15} />
        </span>

        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="truncate text-[13px] font-medium text-text-secondary">
              {invite.email}
            </span>
            <span className="rounded-full bg-black/[0.04] px-2 py-0.5 text-[10px] font-medium text-text-tertiary">
              {invite.role}
            </span>
          </div>
          <div className="mt-0.5 text-[11px] text-text-tertiary">
            Pending · invited {timeAgo(invite.created_at)}
          </div>
        </div>

        <div className="flex shrink-0 items-center gap-1">
          <button
            type="button"
            onClick={onResend}
            disabled={busy}
            title="Rotate the token and get a fresh link"
            className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium text-text-tertiary transition-colors hover:bg-black/[0.04] hover:text-text-secondary disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
          >
            <RotateCw size={13} />
            Resend
          </button>
          <button
            type="button"
            onClick={onRevoke}
            disabled={busy}
            title="Revoke this invite"
            className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium text-text-tertiary transition-colors hover:bg-dismiss/[0.06] hover:text-dismiss disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
          >
            <X size={13} />
            Revoke
          </button>
        </div>
      </div>

      {freshLink && (
        <div className="mt-2.5 pl-[52px]">
          <p className="mb-1.5 text-[11px] text-text-tertiary">
            New link — the previous one no longer works.
          </p>
          <CopyLink url={freshLink} />
        </div>
      )}
    </li>
  )
}
