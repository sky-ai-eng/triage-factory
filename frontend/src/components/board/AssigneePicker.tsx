import { useEffect, useRef, useState } from 'react'
import { Bot } from 'lucide-react'
import type { Task, TeamMember, TeamBot } from '../../types'

// AssigneePicker is SKY-330's per-card assignee selector. Replaces
// the drag-to-Agent gesture that broke when the Agent column was
// removed. Order is fixed: Me, Bot (when enabled), [teammates...].
// Supports self-assign, self-unassign, delegate-to-bot, and (TFAC-561)
// user↔user reassignment via the teammate rows.
//
// State semantics (toggle behavior):
//   - unclaimed + click Me           → claim self
//   - claimed by me + click Me       → unclaim (calls /requeue, which
//                                      clears both claim cols + resets
//                                      status to queued — same path
//                                      the drag-to-Queue gesture uses)
//   - claimed by bot + click Me      → take over (claim self, displaces bot)
//   - claimed by user + click Bot    → delegate (opens prompt picker
//                                      via onDelegateRequested; flips
//                                      claim from user → agent through
//                                      the existing /swipe delegate path)
//   - claimed by bot + click Bot     → no-op (already there; would just
//                                      prompt for a duplicate run)
//   - claimed by a user (me or someone else) + click a teammate row →
//                                      reassign (TFAC-561; the existing
//                                      claimant hands off, or a team
//                                      admin overrides — the server is
//                                      the enforcement boundary, see
//                                      onReassign)
//   - unclaimed or bot-claimed + teammate row →
//                                      disabled; the row explains why
//                                      (claim it, or take it over from
//                                      the bot, first) rather than
//                                      rendering as a dead control
//
// Errors: the onClaim / onUnclaim / onDelegate / onReassign callbacks
// own failure handling. The picker collapses immediately on action
// without waiting for the promise to resolve — if the caller
// surfaces failures via toasts or refetch reconciliation, that's
// where the user sees them. (Pre-merge the docstring referenced
// an `onError` callback that doesn't exist; that wording predated
// the wire-up.)
interface Props {
  task: Task
  currentUserID: string
  members: TeamMember[]
  bot: TeamBot | null
  onClaim: (task: Task) => Promise<void>
  onUnclaim: (task: Task) => Promise<void>
  onDelegate: (task: Task) => void
  onReassign: (task: Task, targetUserID: string) => Promise<void>
  // SKY-330: terminal tasks (done/dismissed) skip the toggle UI —
  // the picker still renders the avatar showing who finished it for
  // audit/history but ignores clicks. Caller passes true for tasks
  // in the Done column.
  readOnly?: boolean
}

export default function AssigneePicker({
  task,
  currentUserID,
  members,
  bot,
  onClaim,
  onUnclaim,
  onDelegate,
  onReassign,
  readOnly = false,
}: Props) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  // Close on outside click. Picker is a small popover; full overlay
  // backdrop would intercept drag gestures on the surrounding card.
  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  const claimedByMe = task.claimed_by_user_id === currentUserID && currentUserID !== ''
  const claimedByOtherUser = !!task.claimed_by_user_id && task.claimed_by_user_id !== currentUserID
  const claimedByBot = !!task.claimed_by_agent_id

  // Resolve the "who's on this" entry for the avatar display.
  const currentAssignee = resolveAssignee(task, members, bot)

  const handleMe = async () => {
    setOpen(false)
    if (claimedByMe) {
      await onUnclaim(task)
    } else {
      // Covers unclaimed + bot-claimed (takeover) + cross-user
      // claim-takeover (the backend's ClaimQueuedForUser / takeover
      // path gates on whether the source claim is legitimate to
      // override; v1 just lets the call go and surfaces 409 if
      // refused).
      await onClaim(task)
    }
  }

  const handleBot = () => {
    setOpen(false)
    if (claimedByBot) return // already there; click is a no-op
    onDelegate(task) // parent opens the prompt picker
  }

  const handleReassign = async (targetUserID: string) => {
    setOpen(false)
    await onReassign(task, targetUserID)
  }

  // SKY-330 chip styling: keeps the picker chrome small so it doesn't
  // dominate the card. Avatar is the click target; the dropdown
  // appears below the card and is dismissible by clicking outside.
  return (
    <div ref={ref} className="relative inline-block">
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation()
          if (readOnly) return
          setOpen((v) => !v)
        }}
        onPointerDown={(e) => e.stopPropagation()}
        title={readOnly ? `Finished by ${currentAssignee.label}` : currentAssignee.label}
        className={`inline-flex items-center gap-1 text-[10px] font-medium leading-none transition-colors ${
          readOnly
            ? 'cursor-default text-text-tertiary'
            : 'cursor-pointer text-text-secondary hover:text-text-primary'
        }`}
      >
        <AssigneeAvatar entry={currentAssignee} />
        <span>{currentAssignee.shortLabel}</span>
      </button>

      {open && !readOnly && (
        <div
          className="absolute z-50 mt-1 right-0 min-w-[180px] bg-surface-raised backdrop-blur-xl border border-border-glass rounded-xl shadow-lg shadow-black/[0.08] py-1"
          onClick={(e) => e.stopPropagation()}
          onPointerDown={(e) => e.stopPropagation()}
        >
          {/* Me */}
          <PickerRow
            avatar={<AvatarCircle initials="ME" tone="user" />}
            label={meLabel(members, currentUserID)}
            sublabel={claimedByMe ? 'Click to unassign' : 'Click to claim'}
            onClick={handleMe}
            selected={claimedByMe}
          />

          {/* Bot — only when enabled for this team */}
          {bot && (
            <PickerRow
              avatar={
                <span className="inline-flex h-5 w-5 items-center justify-center rounded-full bg-accent/15 text-accent">
                  <Bot size={12} aria-hidden />
                </span>
              }
              label={bot.display_name || 'Bot'}
              sublabel={
                claimedByBot
                  ? 'Currently delegated'
                  : claimedByMe
                    ? 'Click to delegate (transfers from you)'
                    : 'Click to delegate'
              }
              onClick={handleBot}
              selected={claimedByBot}
              disabled={claimedByBot}
            />
          )}

          {/* Teammates (TFAC-561): the full roster is rendered (matching
              the design's "me, bot, teammates..." order). A row is only
              clickable — reassign — while the task is currently held by
              a user (self or someone else); an unclaimed or bot-claimed
              task can't reassign directly to a teammate (that's what
              claim / takeover are for), so those rows stay disabled but
              explain why rather than rendering as dead controls. The
              current claimant (if it's a teammate) is marked selected.
              Skip the current user — already rendered as "Me". Server
              is the permission boundary (claimant or team admin only —
              see swipeReassign); a caller without either surfaces a 403
              the same way an unauthorized takeover would. */}
          {(() => {
            const teammates = members.filter(
              (m) => !m.is_current_user && m.user_id !== currentUserID,
            )
            if (teammates.length === 0) return null
            const taskHeldByUser = claimedByMe || claimedByOtherUser
            return (
              <>
                <div className="my-1 border-t border-border-subtle" />
                {teammates.map((m) => {
                  const label = m.display_name || m.github_username || m.user_id
                  const isClaimant = task.claimed_by_user_id === m.user_id
                  const clickable = !isClaimant && taskHeldByUser
                  const sublabel = isClaimant
                    ? 'Currently assigned'
                    : !taskHeldByUser
                      ? claimedByBot
                        ? 'Take over from the bot first to hand off'
                        : 'Claim it yourself first to hand off'
                      : claimedByMe
                        ? 'Click to reassign (transfers from you)'
                        : 'Click to reassign'
                  return (
                    <PickerRow
                      key={m.user_id}
                      avatar={<AvatarCircle initials={initialsFor(label)} tone="user" />}
                      label={label}
                      sublabel={sublabel}
                      onClick={clickable ? () => handleReassign(m.user_id) : undefined}
                      selected={isClaimant}
                      disabled={!clickable}
                    />
                  )
                })}
              </>
            )
          })()}
          {/* Cross-user claim by someone NOT in the loaded roster
              (stale members list, or a cross-team claim that slipped
              through SetClaimedByUser): still surface that the task
              is held so the picker doesn't lie about being unclaimed.
              Skipped when claimedByOtherUser resolves to a teammate
              row above (the selected indicator covers it). */}
          {claimedByOtherUser && !members.some((m) => m.user_id === task.claimed_by_user_id) && (
            <PickerRow
              avatar={<AvatarCircle initials={initialsFor(currentAssignee.label)} tone="user" />}
              label={currentAssignee.label}
              sublabel="Claimed by a user outside this team's roster"
              selected
              disabled
            />
          )}
        </div>
      )}
    </div>
  )
}

interface AssigneeEntry {
  label: string
  shortLabel: string
  kind: 'unclaimed' | 'user' | 'bot' | 'unknown'
}

// resolveAssignee maps the task's claim cols into a displayable entry.
// Unclaimed renders as "Unassigned"; the picker still opens to let the
// user claim. Unknown means we have a claim id we couldn't resolve
// against the members list (cross-team caller, stale roster); the
// avatar falls back to initials + the raw id, not silently empty.
function resolveAssignee(task: Task, members: TeamMember[], bot: TeamBot | null): AssigneeEntry {
  if (task.claimed_by_agent_id) {
    const name = bot?.agent_id === task.claimed_by_agent_id ? bot.display_name : 'Bot'
    return { label: name, shortLabel: 'Bot', kind: 'bot' }
  }
  if (task.claimed_by_user_id) {
    const m = members.find((x) => x.user_id === task.claimed_by_user_id)
    if (m) {
      const label = m.display_name || m.github_username || m.user_id
      return {
        label,
        shortLabel: m.is_current_user ? 'You' : firstName(label),
        kind: 'user',
      }
    }
    // Cross-team / stale-roster claimant — show a truncated id so
    // the row isn't silently empty. Matches the docstring above and
    // is debugger-friendly.
    const shortID = task.claimed_by_user_id.slice(0, 8)
    return { label: 'User ' + shortID, shortLabel: shortID, kind: 'unknown' }
  }
  return { label: 'Unassigned', shortLabel: 'Assign', kind: 'unclaimed' }
}

function meLabel(members: TeamMember[], currentUserID: string): string {
  const me = members.find((m) => m.user_id === currentUserID && m.is_current_user)
  if (me) {
    return `Me (${me.display_name || me.github_username || 'you'})`
  }
  return 'Me'
}

function firstName(s: string): string {
  const trimmed = s.trim()
  if (!trimmed) return ''
  const space = trimmed.indexOf(' ')
  return space === -1 ? trimmed : trimmed.slice(0, space)
}

function initialsFor(s: string): string {
  const parts = s.trim().split(/\s+/).filter(Boolean)
  if (parts.length === 0) return '?'
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase()
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
}

function AssigneeAvatar({ entry }: { entry: AssigneeEntry }) {
  switch (entry.kind) {
    case 'bot':
      // A crisp lucide icon (inherits the chip's currentColor + hover) rather
      // than an emoji — emoji glyphs sit low in their line-box and dropped the
      // chip below its row-mates (elapsed / expand) in the agent card header.
      return <Bot size={13} aria-hidden className="shrink-0" />
    case 'user':
      return <AvatarCircle initials={initialsFor(entry.label)} tone="user" small />
    case 'unknown':
      return <AvatarCircle initials="?" tone="user" small />
    case 'unclaimed':
    default:
      return (
        <span className="inline-block w-3.5 h-3.5 rounded-full border border-dashed border-text-tertiary" />
      )
  }
}

function AvatarCircle({
  initials,
  tone,
  small,
}: {
  initials: string
  tone: 'user' | 'bot'
  small?: boolean
}) {
  const size = small ? 'w-3.5 h-3.5 text-[7px]' : 'w-5 h-5 text-[9px]'
  const colors =
    tone === 'bot' ? 'bg-accent/15 text-accent' : 'bg-text-primary/10 text-text-primary'
  return (
    <span
      className={`inline-flex items-center justify-center rounded-full font-semibold ${size} ${colors}`}
      aria-hidden
    >
      {initials}
    </span>
  )
}

function PickerRow({
  avatar,
  label,
  sublabel,
  onClick,
  selected,
  disabled,
}: {
  avatar: React.ReactNode
  label: string
  sublabel?: string
  onClick?: () => void
  selected?: boolean
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={`w-full flex items-center gap-2 px-3 py-2 text-left transition-colors ${
        disabled ? 'cursor-default opacity-60' : 'hover:bg-black/[0.04] cursor-pointer'
      } ${selected ? 'bg-accent/[0.06]' : ''}`}
    >
      <span className="shrink-0">{avatar}</span>
      <span className="flex-1 min-w-0">
        <span className="block text-[12px] font-medium text-text-primary truncate">{label}</span>
        {sublabel && (
          <span className="block text-[10px] text-text-tertiary truncate">{sublabel}</span>
        )}
      </span>
      {selected && (
        <span className="text-accent text-[12px]" aria-hidden>
          ✓
        </span>
      )}
    </button>
  )
}
