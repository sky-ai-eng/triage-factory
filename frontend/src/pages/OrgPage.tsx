import { useMemo } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Users } from 'lucide-react'
import { apiFetch, apiJSON } from '../lib/apiClient'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import MemberRoster from '../components/MemberRoster'
import OrgSettings from './settings/stack/OrgSettings'
import OrgTemplate from './OrgTemplate'
import type { MemberRosterAdapter, RosterMember } from '../hooks/useMemberRoster'

// OrgMemberApiRow is the wire shape of GET /api/orgs/{org}/members rows.
interface OrgMemberApiRow {
  user_id: string
  display_name: string
  github_username: string | null
  jira_account_id: string | null
  role: string
  is_current_user: boolean
}

const ORG_ROLES = ['owner', 'admin', 'member']

// useOrgRosterAdapter is the org-tier MemberRosterAdapter (TFAC-417): role
// vocab owner|admin|member, protected role owner, and the §1 endpoints wired
// through apiClient's org prefix (/api/orgs/{org}/members…). The add affordance
// (invite-by-email) and extra rows are deliberately left empty here — the
// invite ticket fills addAffordance, and the org tier has no bot row. Memoized
// on orgId so MemberRoster's hook doesn't re-fetch every render.
function useOrgRosterAdapter(orgId: string): MemberRosterAdapter {
  return useMemo<MemberRosterAdapter>(
    () => ({
      roles: ORG_ROLES,
      protectedRole: 'owner',
      async fetchMembers(): Promise<RosterMember[]> {
        const data = await apiJSON<{ members: OrgMemberApiRow[] }>('/members', { org: orgId })
        return data.members.map((m) => ({
          userId: m.user_id,
          displayName: m.display_name,
          githubUsername: m.github_username,
          jiraAccountId: m.jira_account_id,
          role: m.role,
          isCurrentUser: m.is_current_user,
        }))
      },
      async changeRole(userId, role) {
        // Mutations return 204 (no body) — apiFetch (not apiJSON) so a missing
        // JSON body isn't parsed; it still throws HttpError on the 409 guard.
        await apiFetch(`/members/${userId}`, {
          org: orgId,
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ role }),
        })
      },
      async remove(userId) {
        await apiFetch(`/members/${userId}`, { org: orgId, method: 'DELETE' })
      },
      async transferOwnership(newOwnerUserId) {
        // Owner-only on the backend (gated on tf.user_owns_org + the
        // guard_org_owner_transfer trigger). 204 on success; apiFetch throws
        // HttpError on 403/409/422 so the picker surfaces the friendly message.
        await apiFetch('/transfer-ownership', {
          org: orgId,
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ new_owner_user_id: newOwnerUserId }),
        })
      },
    }),
    [orgId],
  )
}

type OrgTab = 'people' | 'settings' | 'template'

const TABS: { id: OrgTab; label: string }[] = [
  { id: 'people', label: 'People' },
  { id: 'settings', label: 'Settings' },
  { id: 'template', label: 'Template' },
]

// OrgPage is the multi-mode org surface: the [People · Settings · Template]
// shell. People is the shared roster (TFAC-417); Settings and Template host the
// relocated OrgSettings / OrgTemplate surfaces (TFAC-419) — the org-scoped
// config moved off the global Settings page, and the template new teams inherit
// moved off the standalone Org template route. The active tab is URL-driven so
// the relocated surfaces stay deep-linkable.
export default function OrgPage() {
  const orgId = useActiveOrgId()

  // OrgPage mounts under /orgs/:org_id, so the active org is effectively always
  // resolved; guard the cold-load window so the body never renders without a
  // concrete org. key={orgId} remounts the body on an org switch (the route
  // stays mounted across a :org_id change), refetching the roster for the
  // newly-active org.
  if (!orgId) {
    return <p className="mx-auto max-w-3xl text-[13px] text-text-tertiary">Loading organization…</p>
  }
  return <OrgPageBody key={orgId} orgId={orgId} />
}

// OrgPageBody is the org shell, rendered once the active org is known and keyed
// on orgId by the parent — so switching orgs remounts it from scratch and the
// adapter always gets a concrete orgId. The active tab is URL-driven (?tab=) so
// the relocated Settings/Template surfaces are deep-linkable — the "Org
// template" nav entry points straight at ?tab=template — and survive a refresh.
function OrgPageBody({ orgId }: { orgId: string }) {
  const { isAdmin } = useOrgRole()
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = resolveTab(searchParams.get('tab'), isAdmin)
  // People owns the bare URL (drop the param); the admin tabs carry an explicit
  // ?tab=. replace so flipping tabs doesn't pile up history entries.
  const setTab = (t: OrgTab) => setSearchParams(t === 'people' ? {} : { tab: t }, { replace: true })
  const adapter = useOrgRosterAdapter(orgId)

  // People (the roster) is everyone's; only admins get the relocated Settings +
  // Template surfaces, so non-admins see just the one tab. /org is multi-mode
  // only (no local route), so OrgSettings always renders multi (isLocal=false).
  const tabs = isAdmin ? TABS : TABS.filter((t) => t.id === 'people')

  return (
    // Template is a full binding-graph canvas, so widen the column for it;
    // People + Settings stay a narrow centered settings column.
    <div className={`mx-auto ${tab === 'template' ? 'max-w-6xl' : 'max-w-3xl'}`}>
      <div className="mb-5 flex items-center gap-2.5">
        <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent-soft text-accent">
          <Users size={15} />
        </span>
        <div>
          <h1 className="text-[17px] font-semibold leading-tight text-text-primary">
            Organization
          </h1>
          <p className="text-[11px] leading-tight text-text-tertiary">
            People, settings, and the template new teams inherit.
          </p>
        </div>
      </div>

      <div className="mb-5 flex gap-1 border-b border-border-subtle">
        {tabs.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => setTab(t.id)}
            className={`-mb-px border-b-2 px-3 py-2 text-[13px] font-medium transition-colors ${
              tab === t.id
                ? 'border-accent text-accent'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'people' && <MemberRoster adapter={adapter} canManage={isAdmin} />}
      {tab === 'settings' && <OrgSettings orgId={orgId} isLocal={false} />}
      {tab === 'template' && <OrgTemplate />}
    </div>
  )
}

// resolveTab maps the ?tab= param to a concrete tab. People is the default and
// the floor for non-admins: a stale or hand-typed ?tab=settings|template from a
// member resolves back to People so the admin surfaces never render for them
// (belt-and-suspenders to the tab-strip filter that hides those tabs).
function resolveTab(raw: string | null, isAdmin: boolean): OrgTab {
  if ((raw === 'settings' || raw === 'template') && isAdmin) return raw
  return 'people'
}
