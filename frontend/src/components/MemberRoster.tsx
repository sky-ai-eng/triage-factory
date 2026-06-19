import { useState } from 'react'
import { Check, LogOut, Trash2 } from 'lucide-react'
import { useMemberRoster, type MemberRosterAdapter } from '../hooks/useMemberRoster'

interface MemberRosterProps {
  // adapter MUST be memoized by the caller (see useMemberRoster).
  adapter: MemberRosterAdapter
  // canManage gates the management controls (role <select>, Remove). A
  // non-admin viewer passes false and gets a read-only roster — still able to
  // Leave their own row.
  canManage: boolean
}

// MemberRoster is the shared, adapter-driven membership table reused by the
// org People tab (here) and the team page (TFAC-415). It owns row rendering,
// the last-privileged guard, and the Leave/Remove split; everything tier-
// specific (role vocab, the add control, extra rows) comes through the adapter.
export default function MemberRoster({ adapter, canManage }: MemberRosterProps) {
  const { members, loading, error, pendingId, actionError, changeRole, remove, clearActionError } =
    useMemberRoster(adapter)

  // The last holder of the protected role can't be demoted or removed — the
  // frontend mirror of the backend's last-owner 409 (disables the control so
  // the user never hits the error in the common case).
  const protectedCount = members.filter((m) => m.role === adapter.protectedRole).length

  if (loading) {
    return <p className="text-[13px] text-text-tertiary">Loading members…</p>
  }
  if (error) {
    return <p className="text-[13px] text-dismiss">{error}</p>
  }

  return (
    <div className="space-y-3">
      {actionError && (
        <div className="flex items-start justify-between gap-3 rounded-xl border border-dismiss/30 bg-dismiss/[0.06] px-4 py-2.5">
          <span className="text-[12px] text-dismiss">{actionError}</span>
          <button
            type="button"
            onClick={clearActionError}
            className="text-[11px] text-text-tertiary underline transition-colors hover:text-text-secondary"
          >
            Dismiss
          </button>
        </div>
      )}

      <div className="overflow-hidden rounded-2xl border border-border-subtle">
        <ul className="divide-y divide-border-subtle">
          {members.map((m) => (
            <MemberRow
              key={m.userId}
              member={m}
              roles={adapter.roles}
              canManage={canManage}
              isLastProtected={m.role === adapter.protectedRole && protectedCount <= 1}
              pending={pendingId === m.userId}
              onChangeRole={(role) => changeRole(m.userId, role)}
              onRemove={() => remove(m.userId)}
            />
          ))}
          {adapter.extraRows}
        </ul>
      </div>

      {adapter.addAffordance}
    </div>
  )
}

interface MemberRowProps {
  member: import('../hooks/useMemberRoster').RosterMember
  roles: string[]
  canManage: boolean
  isLastProtected: boolean
  pending: boolean
  onChangeRole: (role: string) => void
  onRemove: () => void
}

function MemberRow({
  member,
  roles,
  canManage,
  isLastProtected,
  pending,
  onChangeRole,
  onRemove,
}: MemberRowProps) {
  // Self role-changes don't go through this control: stepping down / handing
  // off ownership is the ownership-transfer flow (a later ticket), and a
  // self-demote here risks an accidental lock-out. So the <select> is disabled
  // for the caller's own row, the protected last holder, and any non-admin.
  const roleLocked = !canManage || member.isCurrentUser || isLastProtected || pending
  const initial = (member.displayName.trim()[0] ?? '?').toUpperCase()

  return (
    <li className="flex items-center gap-4 px-4 py-3">
      <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-accent-soft text-[13px] font-semibold text-accent">
        {initial}
      </span>

      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-[13px] font-medium text-text-primary">
            {member.displayName || 'Unnamed user'}
          </span>
          {member.isCurrentUser && (
            <span className="rounded-full bg-black/[0.04] px-2 py-0.5 text-[10px] font-medium text-text-tertiary">
              You
            </span>
          )}
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-2">
          <ReadinessBadge
            label="GitHub"
            value={member.githubUsername}
            connectedText={member.githubUsername ? `@${member.githubUsername}` : ''}
          />
          <ReadinessBadge label="Jira" value={member.jiraAccountId} connectedText="Connected" />
        </div>
      </div>

      <label className="sr-only" htmlFor={`role-${member.userId}`}>
        Role for {member.displayName || member.userId}
      </label>
      <select
        id={`role-${member.userId}`}
        value={member.role}
        disabled={roleLocked}
        onChange={(e) => onChangeRole(e.target.value)}
        title={
          isLastProtected
            ? `Can't change the last ${member.role} — assign another first`
            : member.isCurrentUser
              ? "You can't change your own role here"
              : undefined
        }
        className="rounded-lg border border-border-glass bg-surface px-2.5 py-1.5 text-[12px] text-text-secondary transition-colors focus:border-accent/40 focus:outline-none focus:ring-2 focus:ring-accent/30 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {/* Keep the current role selectable even if it's outside the adapter's
            primary vocab (defensive against legacy values). */}
        {(roles.includes(member.role) ? roles : [member.role, ...roles]).map((r) => (
          <option key={r} value={r}>
            {r}
          </option>
        ))}
      </select>

      <div className="w-[88px] shrink-0 text-right">
        {member.isCurrentUser ? (
          <LeaveButton disabled={isLastProtected || pending} onConfirm={onRemove} />
        ) : canManage ? (
          <button
            type="button"
            onClick={onRemove}
            disabled={isLastProtected || pending}
            title={isLastProtected ? `Can't remove the last ${member.role}` : 'Remove from org'}
            className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium text-text-tertiary transition-colors hover:bg-dismiss/[0.06] hover:text-dismiss disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
          >
            <Trash2 size={13} />
            Remove
          </button>
        ) : null}
      </div>
    </li>
  )
}

// LeaveButton is a two-step inline confirm so a misclick doesn't drop the
// caller out of the org. First click arms it; second confirms.
function LeaveButton({ disabled, onConfirm }: { disabled: boolean; onConfirm: () => void }) {
  const [confirming, setConfirming] = useState(false)

  if (confirming) {
    return (
      <span className="inline-flex items-center gap-1">
        <button
          type="button"
          onClick={() => {
            setConfirming(false)
            onConfirm()
          }}
          className="rounded-lg bg-dismiss px-2 py-1.5 text-[11px] font-semibold text-white transition-colors hover:bg-dismiss/90"
        >
          Confirm
        </button>
        <button
          type="button"
          onClick={() => setConfirming(false)}
          className="rounded-lg px-1.5 py-1.5 text-[11px] text-text-tertiary transition-colors hover:text-text-secondary"
        >
          Cancel
        </button>
      </span>
    )
  }

  return (
    <button
      type="button"
      onClick={() => setConfirming(true)}
      disabled={disabled}
      title={
        disabled ? "You're the last owner — assign another owner before leaving" : 'Leave this org'
      }
      className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium text-text-tertiary transition-colors hover:bg-dismiss/[0.06] hover:text-dismiss disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
    >
      <LogOut size={13} />
      Leave
    </button>
  )
}

// ReadinessBadge renders a host-scoped identity's connected/not-connected
// state, mirroring the UserSettings "connected" affordance: a claim-colored
// chip when bound, muted text when null.
function ReadinessBadge({
  label,
  value,
  connectedText,
}: {
  label: string
  value: string | null
  connectedText: string
}) {
  if (!value) {
    return (
      <span className="inline-flex items-center gap-1 rounded-full bg-black/[0.03] px-2 py-0.5 text-[10px] text-text-tertiary">
        {label}: Not connected
      </span>
    )
  }
  return (
    <span className="inline-flex items-center gap-1 rounded-full border border-claim/15 bg-claim/[0.06] px-2 py-0.5 text-[10px] text-claim">
      <Check size={10} />
      {label}
      {connectedText ? `: ${connectedText}` : ''}
    </span>
  )
}
