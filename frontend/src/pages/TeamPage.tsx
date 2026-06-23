import { useMemo, useState } from 'react'
import { Users } from 'lucide-react'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import { useTeams } from '../hooks/useTeams'
import TeamMembersPanel from '../components/TeamMembersPanel'

// TeamPage is a TEMPORARY mount for the team roster (TFAC-444): it lets the new
// member CRUD + <TeamMembersPanel> be exercised end-to-end before the real
// /team page shell lands (TFAC-445), which replaces this with the
// [Members · Settings · Prompts] tabs + a shared team switcher. Multi-mode only
// (mounted under /orgs/:org_id). It resolves the caller's sticky/default team
// and offers a bare <select> to switch when they're on more than one.
export default function TeamPage() {
  const orgId = useActiveOrgId()
  const { isAdmin: orgIsAdmin } = useOrgRole()
  const { teams, lastActingTeamId, loaded, loading, error } = useTeams()

  // Default selection: the sticky last-acting team when it's still one of the
  // caller's teams, else the org's default (teams[0], oldest-first).
  const defaultTeamId = useMemo(() => {
    if (teams.some((t) => t.id === lastActingTeamId)) return lastActingTeamId
    return teams[0]?.id ?? ''
  }, [teams, lastActingTeamId])

  const [picked, setPicked] = useState('')
  const teamId = picked && teams.some((t) => t.id === picked) ? picked : defaultTeamId
  const team = teams.find((t) => t.id === teamId)
  // canManage: a team admin OR an org admin — the same union the backend gate
  // enforces. The roster re-checks server-side, so this is just the UI gate.
  const canManage = !!team && (team.role === 'admin' || orgIsAdmin)

  if (!orgId || loading || !loaded) {
    return <p className="mx-auto max-w-3xl text-[13px] text-text-tertiary">Loading teams…</p>
  }
  if (error) {
    return <p className="mx-auto max-w-3xl text-[13px] text-dismiss">{error}</p>
  }
  // Zero-team state — a user who's in the org but on no team. The full
  // safe-landing treatment is the page-shell slice's; here we just say so.
  if (!team) {
    return (
      <p className="mx-auto max-w-3xl text-[13px] text-text-tertiary">
        You're not on any team yet.
      </p>
    )
  }

  return (
    <div className="mx-auto max-w-3xl">
      <div className="mb-5 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent-soft text-accent">
            <Users size={15} />
          </span>
          <div>
            <h1 className="text-[17px] font-semibold leading-tight text-text-primary">
              {team.name}
            </h1>
            <p className="text-[11px] leading-tight text-text-tertiary">Team members</p>
          </div>
        </div>
        {teams.length > 1 && (
          <select
            value={teamId}
            onChange={(e) => setPicked(e.target.value)}
            aria-label="Switch team"
            className="rounded-lg border border-border-glass bg-surface px-2.5 py-1.5 text-[12px] text-text-secondary focus:border-accent/40 focus:outline-none focus:ring-2 focus:ring-accent/30"
          >
            {teams.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
        )}
      </div>

      {/* key remounts the panel on a team switch — fresh roster + bot. */}
      <TeamMembersPanel key={teamId} orgId={orgId} teamId={teamId} canManage={canManage} />
    </div>
  )
}
