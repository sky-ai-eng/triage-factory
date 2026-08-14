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
//   • Team  — LOCAL MODE ONLY. Multi mode relocates the team-scoped sections
//             (repos, GitHub teams, Jira projects, team defaults) to the /team
//             page's Settings tab (TFAC-445); /team has no local route, so N=1
//             keeps them here (admin of its sole team, addressed as "default").
//   • User  — always (personal identity + device prefs).
// Both relocated groups render here only in local mode — N=1 is admin of
// everything, so its groups render with no selector and no role probes.

import { GlassBackdrop } from './setup/glass'
import { SectionDivider } from './setup/parts'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
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

  // Multi mode whose active org hasn't resolved yet — hold rather than fetch
  // the wrong org's state.
  if (!isLocal && !orgId) {
    return (
      <div className="flex min-h-[50vh] items-center justify-center">
        <p className="text-body text-ink-3">Loading settings…</p>
      </div>
    )
  }

  return (
    <div className="relative min-h-full px-4 py-10">
      <GlassBackdrop />
      <div className="mx-auto max-w-2xl">
        <h1 className="mb-8 text-[22px] font-semibold tracking-tight text-ink-1">
          Settings
        </h1>

        <div className="space-y-8">
          {/* Org + Team groups — local mode only. Multi mode relocates Org to
              the /org page and Team to the /team page's Settings tab; neither
              has a local route, so N=1 (always admin of its sole org + team)
              edits both here. */}
          {isLocal && (
            <>
              <section aria-labelledby="settings-section-org">
                <SectionDivider id="settings-section-org" title="Organization" />
                <OrgSettings orgId={orgId} isLocal={isLocal} />
              </section>

              <section aria-labelledby="settings-section-team">
                <SectionDivider id="settings-section-team" title="Team" />
                <TeamSettings isLocal={isLocal} teamId="default" />
              </section>
            </>
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
