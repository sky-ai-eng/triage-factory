import { useEffect, useRef, useState } from 'react'
import { Plus, UserPlus } from 'lucide-react'
import { apiFetch, apiList, httpErrorMessage } from '../lib/apiClient'
import { fetchTeamRoster } from '../lib/teamRoster'
import { TEAM_ROLE_LABELS } from '../lib/teamRoles'
import { useFocusTrap } from '../hooks/useFocusTrap'

// TeamMemberPicker is the team roster's "add member" affordance (TFAC-444): a
// button that opens a modal listing the org's members who AREN'T already on the
// team (the member-picker source — pick from existing org members, no email).
// Picking one + a role POSTs to /api/teams/{team}/members; on success it calls
// onAdded so the parent reloads the roster.
interface TeamMemberPickerProps {
  orgId: string
  teamId: string
  onAdded: () => void
}

interface OrgMemberApiRow {
  user_id: string
  display_name: string
  github_username: string | null
  role: string
}

// 'member' leads — the common add. The roster's own <select> orders these
// admin-first; the picker's order is its own, but the labels come from the
// shared map so the two surfaces can't describe the same role differently.
const ADD_ROLES = ['member', 'admin', 'viewer']

export default function TeamMemberPicker({ orgId, teamId, onAdded }: TeamMemberPickerProps) {
  const [open, setOpen] = useState(false)
  return (
    <div className="flex justify-end pt-1">
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="inline-flex items-center gap-1.5 rounded-full border border-line-1 bg-raised px-3.5 py-2 text-body font-medium text-ink-2 transition-colors hover:bg-sunk hover:text-ink-1"
      >
        <Plus size={14} />
        Add member
      </button>
      {open && (
        <PickerModal
          orgId={orgId}
          teamId={teamId}
          onAdded={onAdded}
          onClose={() => setOpen(false)}
        />
      )}
    </div>
  )
}

function PickerModal({
  orgId,
  teamId,
  onAdded,
  onClose,
}: TeamMemberPickerProps & { onClose: () => void }) {
  // candidates: org members not yet on the team. null = still loading.
  const [candidates, setCandidates] = useState<OrgMemberApiRow[] | null>(null)
  const [selected, setSelected] = useState('')
  const [role, setRole] = useState('member')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2).
  const dialogRef = useRef<HTMLDivElement>(null)
  useFocusTrap(dialogRef)

  // Compute "org members not on the team" from the two rosters: the org roster
  // (the source set) minus everyone already on the team.
  useEffect(() => {
    let alive = true
    Promise.all([
      // page_size at the route max: the picker's candidate set is "org
      // members minus team members", which needs both rosters whole. An org
      // past 200 members would need a search-as-you-type picker rather than a
      // longer page.
      apiList<OrgMemberApiRow>(`/api/orgs/${encodeURIComponent(orgId)}/members/list`, {
        page_size: 200,
      }),
      fetchTeamRoster(teamId),
    ])
      .then(([org, team]) => {
        if (!alive) return
        const onTeam = new Set(team.members.map((m) => m.user_id))
        const addable = org.items.filter((m) => !onTeam.has(m.user_id))
        setCandidates(addable)
        setSelected(addable[0]?.user_id ?? '')
      })
      .catch((e) => {
        if (alive) setError(httpErrorMessage(e, 'Could not load org members.'))
      })
    return () => {
      alive = false
    }
  }, [orgId, teamId])

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
      await apiFetch(`/api/teams/${teamId}/members`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ user_id: selected, role }),
      })
      onAdded()
      onClose()
    } catch (e) {
      // The 409 (already a member) / 422 (not an org member) arrive verbatim
      // from the backend; show them in place.
      setError(httpErrorMessage(e, 'Could not add member.'))
    } finally {
      setSubmitting(false)
    }
  }

  const empty = candidates !== null && candidates.length === 0

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
        aria-labelledby="add-team-member-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 pb-4 pt-6">
          <div className="flex items-center gap-2.5">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-warm-2 text-warm">
              <UserPlus size={15} />
            </span>
            <h2
              id="add-team-member-title"
              className="text-section font-semibold tracking-tight text-ink-1"
            >
              Add a team member
            </h2>
          </div>
          <p className="mt-2 text-body leading-relaxed text-ink-3">
            Pick someone already in the org and choose their team role.
          </p>
        </div>

        <div className="min-h-0 flex-1 space-y-3 overflow-y-auto px-6">
          {candidates === null && !error && (
            <p className="py-2 text-body text-ink-3">Loading org members…</p>
          )}
          {empty && (
            <p className="py-2 text-body text-ink-3">
              Everyone in the org is already on this team.
            </p>
          )}
          {candidates !== null && candidates.length > 0 && (
            <>
              <label className="block">
                <span className="mb-1 block text-ui font-medium text-ink-2">Member</span>
                <select
                  value={selected}
                  disabled={submitting}
                  onChange={(e) => setSelected(e.target.value)}
                  className="w-full rounded-lg border border-line-1 bg-ground px-2.5 py-2 text-body text-ink-2 focus:border-warm/40 focus:outline-none focus:ring-2 focus:ring-warm/30 disabled:opacity-50"
                >
                  {candidates.map((m) => (
                    <option key={m.user_id} value={m.user_id}>
                      {m.display_name ||
                        (m.github_username ? `@${m.github_username}` : 'Unnamed user')}
                    </option>
                  ))}
                </select>
              </label>
              <label className="block">
                <span className="mb-1 block text-ui font-medium text-ink-2">Role</span>
                <select
                  value={role}
                  disabled={submitting}
                  onChange={(e) => setRole(e.target.value)}
                  className="w-full rounded-lg border border-line-1 bg-ground px-2.5 py-2 text-body text-ink-2 focus:border-warm/40 focus:outline-none focus:ring-2 focus:ring-warm/30 disabled:opacity-50"
                >
                  {ADD_ROLES.map((r) => (
                    <option key={r} value={r}>
                      {TEAM_ROLE_LABELS[r] ?? r}
                    </option>
                  ))}
                </select>
              </label>
            </>
          )}
        </div>

        {error && <p className="px-6 pt-2 text-ui text-alarm">{error}</p>}

        <div className="flex items-center justify-end gap-3 border-t border-line-1 px-6 py-4">
          <button
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
            disabled={!selected || submitting || empty}
            className="rounded-xl bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40"
          >
            {submitting ? 'Adding…' : 'Add member'}
          </button>
        </div>
      </div>
    </div>
  )
}
