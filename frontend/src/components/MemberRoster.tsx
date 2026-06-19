import { useMemo, useState } from 'react'
import { ArrowLeftRight, Check, LogOut, Trash2 } from 'lucide-react'
import { useMemberRoster, type MemberRosterAdapter } from '../hooks/useMemberRoster'
import TransferOwnershipModal from './TransferOwnershipModal'

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
  const {
    members,
    loading,
    error,
    pendingId,
    actionError,
    changeRole,
    remove,
    reload,
    clearActionError,
  } = useMemberRoster(adapter)

  // transfer holds the open transfer-ownership modal's reason, or null when
  // closed. 'manual' = the owner clicked "Transfer ownership"; 'leave' = the
  // sole owner clicked Leave and must hand off first.
  const [transfer, setTransfer] = useState<'manual' | 'leave' | null>(null)

  // The last holder of the protected role can't be demoted or removed — the
  // frontend mirror of the backend's last-owner 409 (disables the control so
  // the user never hits the error in the common case).
  const protectedCount = members.filter((m) => m.role === adapter.protectedRole).length

  // Ownership transfer is owner-only and org-only: the adapter supplies
  // transferOwnership (org yes, team no), the viewer must hold the protected
  // role, and there must be someone to hand off to. The viewer's own row
  // carries their role, so owner-ness is derived from the list rather than a
  // second fetch. The backend re-checks ownership (tf.user_owns_org), so this
  // is just the UI gate.
  const viewer = members.find((m) => m.isCurrentUser)
  const viewerIsOwner = !!viewer && viewer.role === adapter.protectedRole
  const transferTargets = useMemo(() => members.filter((m) => !m.isCurrentUser), [members])
  const canTransfer = !!adapter.transferOwnership && viewerIsOwner && transferTargets.length > 0

  // busy = a row mutation (role change / remove) is in flight. The hook
  // serializes to one change at a time; every mutating affordance — including
  // both transfer entry points (the action below and the sole-owner Leave) —
  // disables on it so a second write can't start and race the first's refetch.
  const busy = pendingId !== null

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

      {canTransfer && (
        <div className="flex justify-end">
          <button
            type="button"
            onClick={() => setTransfer('manual')}
            // Transfer is itself a mutation — disable it while another row
            // change is in flight so it can't race the hook's serialized writes.
            disabled={busy}
            title={busy ? 'Another change is in progress…' : undefined}
            className="inline-flex items-center gap-1.5 rounded-lg border border-border-glass bg-surface px-3 py-1.5 text-[12px] font-medium text-text-secondary transition-colors hover:border-accent/40 hover:text-accent disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:border-border-glass disabled:hover:text-text-secondary"
          >
            <ArrowLeftRight size={13} />
            Transfer ownership
          </button>
        </div>
      )}

      <div className="overflow-hidden rounded-2xl border border-border-subtle">
        <ul className="divide-y divide-border-subtle">
          {members.map((m) => {
            const isLastProtected = m.role === adapter.protectedRole && protectedCount <= 1
            // A sole owner clicking Leave shouldn't hit a dead disabled button
            // (the backend would 409): route them to "transfer ownership
            // first" instead — but only when there's an eligible recipient
            // (canTransfer). With nobody to hand off to, Leave stays disabled;
            // dissolving the org is a separate concern.
            const leaveNeedsTransfer = m.isCurrentUser && isLastProtected && canTransfer
            return (
              <MemberRow
                key={m.userId}
                member={m}
                roles={adapter.roles}
                canManage={canManage}
                isLastProtected={isLastProtected}
                busy={busy}
                onChangeRole={(role) => changeRole(m.userId, role)}
                onRemove={() => remove(m.userId)}
                onLeaveNeedsTransfer={leaveNeedsTransfer ? () => setTransfer('leave') : undefined}
              />
            )
          })}
          {adapter.extraRows}
        </ul>
      </div>

      {adapter.addAffordance}

      {transfer && adapter.transferOwnership && (
        <TransferOwnershipModal
          candidates={transferTargets}
          reason={transfer}
          transfer={adapter.transferOwnership}
          onDone={() => {
            setTransfer(null)
            reload()
          }}
          onClose={() => setTransfer(null)}
        />
      )}
    </div>
  )
}

interface MemberRowProps {
  member: import('../hooks/useMemberRoster').RosterMember
  roles: string[]
  canManage: boolean
  isLastProtected: boolean
  // busy is true while ANY row's mutation is in flight — every row's
  // mutating control disables, so a second mutation can't start and race the
  // first's refetch/error (the hook serializes to one change at a time).
  busy: boolean
  onChangeRole: (role: string) => void
  onRemove: () => void
  // onLeaveNeedsTransfer, when set, marks this (current-user) row as the sole
  // owner: Leave routes to the transfer-ownership picker instead of removing
  // the row. Undefined for everyone else.
  onLeaveNeedsTransfer?: () => void
}

function MemberRow({
  member,
  roles,
  canManage,
  isLastProtected,
  busy,
  onChangeRole,
  onRemove,
  onLeaveNeedsTransfer,
}: MemberRowProps) {
  // Self role-changes don't go through this control: stepping down / handing
  // off ownership is the dedicated ownership-transfer flow (the "Transfer
  // ownership" action, or the sole-owner Leave prompt), and a self-demote here
  // risks an accidental lock-out. So the <select> is disabled for the caller's
  // own row, the protected last holder, any non-admin, and while any mutation
  // is in flight (busy) — one change at a time.
  const roleLocked = !canManage || member.isCurrentUser || isLastProtected || busy
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
          <LeaveButton
            busy={busy}
            disabled={isLastProtected || busy}
            onConfirm={onRemove}
            onNeedsTransfer={onLeaveNeedsTransfer}
          />
        ) : canManage ? (
          <button
            type="button"
            onClick={onRemove}
            disabled={isLastProtected || busy}
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
//
// onNeedsTransfer overrides that entirely for the sole owner: a single click
// opens the transfer-ownership picker (no destructive removal happens here, so
// no two-step confirm) instead of leaving the button disabled at a dead 409.
function LeaveButton({
  busy,
  disabled,
  onConfirm,
  onNeedsTransfer,
}: {
  busy: boolean
  disabled: boolean
  onConfirm: () => void
  onNeedsTransfer?: () => void
}) {
  const [confirming, setConfirming] = useState(false)

  if (onNeedsTransfer) {
    // Sole owner: Leave opens the transfer picker instead of removing the row.
    // Opening it starts a transfer (a mutation), so it stays disabled while
    // another change is in flight (busy) — but NOT on `disabled`'s
    // isLastProtected component, since being the last owner is exactly the
    // case this path exists to handle.
    return (
      <button
        type="button"
        onClick={onNeedsTransfer}
        disabled={busy}
        title={busy ? 'Another change is in progress…' : 'Transfer ownership before leaving'}
        className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium text-text-tertiary transition-colors hover:bg-dismiss/[0.06] hover:text-dismiss disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-tertiary"
      >
        <LogOut size={13} />
        Leave
      </button>
    )
  }

  if (confirming) {
    return (
      <span className="inline-flex items-center gap-1">
        <button
          type="button"
          // Gate Confirm on disabled too: if the user armed Leave and then
          // became the last owner (or a mutation started), the armed Confirm
          // must not stay clickable. Cancel stays enabled so they can back out.
          disabled={disabled}
          onClick={() => {
            setConfirming(false)
            onConfirm()
          }}
          className="rounded-lg bg-dismiss px-2 py-1.5 text-[11px] font-semibold text-white transition-colors hover:bg-dismiss/90 disabled:cursor-not-allowed disabled:opacity-40"
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
