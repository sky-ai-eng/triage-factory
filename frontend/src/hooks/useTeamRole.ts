import { useCallback, useMemo } from 'react'
import { useTeams } from './useTeams'

// Team roles that may perform team-scoped writes — admin + member, mirroring
// the backend's tf.user_can_write_team (member exists AND role IN
// ('admin','member')). A viewer is a read-only member: present in the roster,
// excluded from every team-scoped write (TFAC-447).
const WRITE_ROLES = new Set(['admin', 'member'])

export interface TeamRoleState {
  /** The viewer's role in teamId ("admin" | "member" | "viewer"), or null when
   *  unknown — still loading, or the team isn't one of theirs. */
  roleForTeam: (teamId: string | undefined) => string | null
  /** Whether the viewer may perform team-scoped writes (delegate, swipe, edit
   *  prompts, create) on teamId. False only when their role is a known viewer;
   *  unknown/loading defaults to permissive so a member isn't wrongly gated mid-
   *  load (the backend RLS is the hard guarantee — this only governs whether we
   *  *offer* the affordance). */
  canWriteTeam: (teamId: string | undefined) => boolean
  /** Convenience: the viewer is a known viewer of teamId (the inverse of
   *  canWriteTeam for a resolved role). Drives the "view-only" indicator. */
  isViewer: (teamId: string | undefined) => boolean
  /** True once the first /api/teams fetch resolved — gate any redirect/hide on
   *  this so an affordance doesn't flicker before roles are known. */
  loaded: boolean
}

// useTeamRole reports the viewer's per-team role, derived from the shared
// /api/teams cache (which already carries the caller's membership role per team
// — see teamJSON.Role). It's the team-tier counterpart to useOrgRole: team-
// scoped surfaces consume it to hide or disable mutation affordances for
// viewers. Safe in both modes — local/N=1's sole team reports "admin", so every
// gate reads writable there and no affordance is hidden.
export function useTeamRole(): TeamRoleState {
  const { teams, loaded } = useTeams()

  const byId = useMemo(() => {
    const m = new Map<string, string>()
    for (const t of teams) m.set(t.id, t.role)
    return m
  }, [teams])

  const roleForTeam = useCallback(
    (teamId: string | undefined): string | null => {
      if (!teamId) return null
      return byId.get(teamId) ?? null
    },
    [byId],
  )

  const canWriteTeam = useCallback(
    (teamId: string | undefined): boolean => {
      const role = roleForTeam(teamId)
      // Unknown (loading, or a team not in my list — e.g. an org-level surface
      // that isn't team-scoped) → permissive. We only ever *withhold* an
      // affordance for a role we positively know to be read-only.
      if (role == null) return true
      return WRITE_ROLES.has(role)
    },
    [roleForTeam],
  )

  const isViewer = useCallback(
    (teamId: string | undefined): boolean => {
      const role = roleForTeam(teamId)
      return role != null && !WRITE_ROLES.has(role)
    },
    [roleForTeam],
  )

  return { roleForTeam, canWriteTeam, isViewer, loaded }
}
