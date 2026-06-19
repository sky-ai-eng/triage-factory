// Settings — the non-progressive sibling of the setup wizard. Same liquid-glass
// material (GlassBackdrop + the section dividers), but every section the
// viewer is allowed to see renders at once, collapsed by default; expanding one
// leaves its neighbours — before AND after — collapsed and visible, and each
// section saves independently. No linear flow, no "road ahead" hiding.
//
// Sections are role-gated, mirroring the wizard's Org → Team → User order:
//   • Org   — LOCAL MODE ONLY. Multi mode relocates the org-scoped sections
//             (GitHub/Jira connection, polling, model cap, Claude credentials,
//             danger zone) + the org template to the dedicated /org page
//             (TFAC-419); but /org has no local route, so N=1 keeps them here —
//             Settings is local mode's only post-setup org-config surface.
//   • Team  — rendered when the viewer admins ≥1 team; a selector (admin'd
//             teams only) picks which one. Subsumes per-non-default-team config.
//   • User  — always (personal identity + device prefs).
// The team gate mirrors the backend's role gates: an org admin does NOT inherit
// team-admin, so a team they don't admin won't appear in their Team selector.
// Local / N=1 is admin of everything, so its groups render with no selector and
// no role probes.

import { GlassBackdrop } from './setup/glass'
import { SectionDivider } from './setup/parts'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { useTeams } from '../hooks/useTeams'
import OrgSettings from './settings/stack/OrgSettings'
import TeamSettings from './settings/stack/TeamSettings'
import UserSettings from './settings/stack/UserSettings'

export default function Settings() {
  // useOptionalAuth is null in local mode (no AuthProvider) — the degenerate
  // single-user world, admin of everything. isLocal both tells local from multi
  // and gates the Org group: multi relocates it to /org, local (no /org route)
  // keeps it here.
  const auth = useOptionalAuth()
  const isLocal = auth === null
  // Org-scoped endpoints (the App panel, identity) take the id in the path.
  // Local has no OrgContext, so it uses the sentinel; multi uses the resolved
  // active org and is null until it resolves.
  const ctxOrgId = useActiveOrgId()
  const orgId = isLocal ? LOCAL_DEFAULT_ORG_ID : ctxOrgId

  const { teams, lastActingTeamId, loaded: teamsLoaded } = useTeams()
  // The teams the viewer admins — gates the Team group + filters its selector.
  // Local reports "admin" for its sole team, so this is non-empty there too.
  const adminTeams = teams.filter((t) => t.role === 'admin')
  const showTeam = adminTeams.length > 0

  // Multi mode whose active org hasn't resolved yet — hold rather than fetch
  // the wrong org's state.
  if (!isLocal && !orgId) {
    return (
      <div className="flex min-h-[50vh] items-center justify-center">
        <p className="text-[13px] text-text-tertiary">Loading settings…</p>
      </div>
    )
  }

  return (
    <div className="relative min-h-screen px-4 py-10">
      <GlassBackdrop />
      <div className="mx-auto max-w-2xl">
        <h1 className="mb-8 text-[22px] font-semibold tracking-tight text-text-primary">
          Settings
        </h1>

        <div className="space-y-8">
          {/* Org group — local mode only. Multi mode relocates it to the /org
              page; local has no /org route, so N=1 edits org config here (it's
              always org admin, so isLocal alone gates it). */}
          {isLocal && (
            <section aria-labelledby="settings-section-org">
              <SectionDivider id="settings-section-org" title="Organization" />
              <OrgSettings orgId={orgId} isLocal={isLocal} />
            </section>
          )}

          {/* Render the Team group once teams resolve and the viewer admins
              one — keeps it from flashing in before the role set is known. */}
          {teamsLoaded && showTeam && (
            <section aria-labelledby="settings-section-team">
              <SectionDivider id="settings-section-team" title="Team" />
              <TeamSettings
                isLocal={isLocal}
                adminTeams={adminTeams}
                lastActingTeamId={lastActingTeamId}
              />
            </section>
          )}

          <section aria-labelledby="settings-section-user">
            <SectionDivider id="settings-section-user" title="User" />
            <UserSettings orgId={orgId} />
          </section>
        </div>
      </div>
    </div>
  )
}
