import { useMemo, useState } from 'react'
import { Users } from 'lucide-react'
import { apiFetch, apiJSON } from '../lib/apiClient'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import MemberRoster from '../components/MemberRoster'
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
// shell. This ticket (TFAC-417) ships the shell + the People tab (the shared
// roster); the Settings and Template tabs are placeholders the relocation
// ticket fills with the existing OrgSettings / OrgTemplate content.
export default function OrgPage() {
  const orgId = useActiveOrgId()

  // OrgPage mounts under /orgs/:org_id, so the active org is effectively always
  // resolved; guard the cold-load window so the body never renders without a
  // concrete org. key={orgId} remounts the body on an org switch (the route
  // stays mounted across a :org_id change), which resets the selected tab and
  // refetches the roster for the newly-active org.
  if (!orgId) {
    return <p className="mx-auto max-w-3xl text-[13px] text-text-tertiary">Loading organization…</p>
  }
  return <OrgPageBody key={orgId} orgId={orgId} />
}

// OrgPageBody is the org shell, rendered once the active org is known and keyed
// on orgId by the parent — so switching orgs remounts it from scratch, with no
// stale tab selection or roster carried across, and the adapter always gets a
// concrete orgId.
function OrgPageBody({ orgId }: { orgId: string }) {
  const { isAdmin } = useOrgRole()
  const [tab, setTab] = useState<OrgTab>('people')
  const adapter = useOrgRosterAdapter(orgId)

  return (
    <div className="mx-auto max-w-3xl">
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
        {TABS.map((t) => (
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

      {tab === 'settings' && (
        <TabStub title="Org settings move here">
          Connection, polling, model cap, Claude credentials, and the danger zone relocate to this
          tab in a follow-up. For now they remain under Settings.
        </TabStub>
      )}

      {tab === 'template' && (
        <TabStub title="Org template moves here">
          The org-wide prompt graph new teams inherit relocates to this tab in a follow-up. For now
          it remains on its own Org template page.
        </TabStub>
      )}
    </div>
  )
}

// TabStub is the placeholder body for the Settings / Template tabs until the
// relocation ticket moves their real content here.
function TabStub({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-2xl border border-dashed border-border-subtle px-5 py-8 text-center">
      <p className="text-[13px] font-medium text-text-secondary">{title}</p>
      <p className="mx-auto mt-1.5 max-w-md text-[12px] leading-relaxed text-text-tertiary">
        {children}
      </p>
    </div>
  )
}
