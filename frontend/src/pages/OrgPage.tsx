import { useCallback, useMemo, useState } from 'react'
import { Plus, Users } from 'lucide-react'
import * as Switch from '@radix-ui/react-switch'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import { useInvites } from '../hooks/useInvites'
import MemberRoster from '../components/MemberRoster'
import InviteModal from '../components/InviteModal'
import PendingInviteRow from '../components/PendingInviteRow'
import { toast } from '../components/Toast/toastStore'
import type { MemberRosterAdapter, RosterMember } from '../hooks/useMemberRoster'
import type { PendingInvite } from '../types'

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

// useOrgRosterAdapter is the org-tier MemberRosterAdapter's DATA layer
// (TFAC-417): role vocab owner|admin|member, protected role owner, and the
// member endpoints wired through apiClient's org prefix (/api/orgs/{org}/
// members…). addAffordance + extraRows are left empty here and composed on top
// by OrgPeople (TFAC-418) — the invite modal slot + the pending ghost rows.
// Memoized on orgId so MemberRoster's hook doesn't re-fetch every render; the
// composed wrapper keeps these fetch/mutate methods referentially stable so
// adding the live invite slots can't trigger a roster refetch loop.
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

      {tab === 'people' && <OrgPeople orgId={orgId} canManage={isAdmin} />}

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

// OrgPeople is the People tab body: the shared member roster plus the
// org-adapter-specific invite surface (TFAC-418) — the "+ Invite" modal in the
// roster's addAffordance slot, the pending-invite ghost rows in extraRows, and
// the "Show invited" header toggle. The whole invite surface is gated on
// canManage: non-admins get a 404 on GET /api/invites (non-disclosure), so the
// hook stays inert for them and they see a plain read-only roster.
function OrgPeople({ orgId, canManage }: { orgId: string; canManage: boolean }) {
  const base = useOrgRosterAdapter(orgId)
  const { invites, error, create, revoke, resend } = useInvites(canManage)
  // Default shown (per the ticket). Hidden only collapses the ghost rows; the
  // invites still exist and the "+ Invite" button stays put.
  const [showInvited, setShowInvited] = useState(true)
  const [modalOpen, setModalOpen] = useState(false)
  // The invite id with a revoke/resend in flight — disables that row's controls
  // so a second action can't race the reload. One at a time, like the roster.
  const [busyId, setBusyId] = useState<string | null>(null)
  // Fresh accept links from resends, keyed by the NEW invite id (resend rotates
  // to a new row). Revealed inline on that row so the admin can copy the link.
  const [resentLinks, setResentLinks] = useState<Record<string, string>>({})

  const handleRevoke = useCallback(
    async (id: string) => {
      setBusyId(id)
      try {
        await revoke(id)
      } catch (e) {
        toast.error(httpErrorMessage(e, 'Could not revoke the invite.'))
      } finally {
        setBusyId(null)
      }
    },
    [revoke],
  )

  const handleResend = useCallback(
    async (invite: PendingInvite) => {
      setBusyId(invite.id)
      try {
        const res = await resend(invite)
        setResentLinks((m) => ({ ...m, [res.id]: res.accept_url }))
        toast.success('Invite link rotated — copy the new one below.')
      } catch (e) {
        toast.error(httpErrorMessage(e, 'Could not resend the invite.'))
      } finally {
        setBusyId(null)
      }
    },
    [resend],
  )

  // Only surface fresh links for invites still in the list. Derived (no effect):
  // the rows iterate `invites` anyway, so this just bounds what's read and keeps
  // a long session's resends from passing dead entries down.
  const activeIds = new Set(invites.map((i) => i.id))
  const visibleResentLinks = Object.fromEntries(
    Object.entries(resentLinks).filter(([id]) => activeIds.has(id)),
  )

  // Pending ghost rows for the roster's extraRows slot — only when the viewer
  // can manage and the toggle is on. Plain <li>s so they slot into the <ul>.
  const extraRows =
    canManage && showInvited
      ? invites.map((inv) => (
          <PendingInviteRow
            key={inv.id}
            invite={inv}
            freshLink={visibleResentLinks[inv.id]}
            busy={busyId === inv.id}
            onRevoke={() => void handleRevoke(inv.id)}
            onResend={() => void handleResend(inv)}
          />
        ))
      : null

  const addAffordance = canManage ? (
    <div className="flex justify-end pt-1">
      <button
        type="button"
        onClick={() => setModalOpen(true)}
        className="inline-flex items-center gap-1.5 rounded-full border border-border-subtle bg-white/60 px-3.5 py-2 text-[13px] font-medium text-text-secondary transition-colors hover:bg-white hover:text-text-primary"
      >
        <Plus size={14} />
        Invite
      </button>
    </div>
  ) : null

  // Compose the live invite slots onto the memoized base adapter. The spread is
  // a fresh object each render, but base.fetchMembers/changeRole/remove keep
  // their identity, so useMemberRoster's load effect (keyed on those) never
  // refires from a changing addAffordance/extraRows.
  const adapter: MemberRosterAdapter = { ...base, addAffordance, extraRows }

  return (
    <div className="space-y-3">
      {canManage && invites.length > 0 && (
        <div className="flex items-center justify-end">
          <label className="flex cursor-pointer items-center gap-2 text-[12px] text-text-tertiary">
            Show invited
            <Switch.Root
              checked={showInvited}
              onCheckedChange={setShowInvited}
              className="relative h-[18px] w-8 rounded-full transition-colors data-[state=checked]:bg-accent data-[state=unchecked]:bg-black/10"
            >
              <Switch.Thumb className="block h-[14px] w-[14px] rounded-full bg-white shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
            </Switch.Root>
          </label>
        </div>
      )}

      {canManage && error && (
        <p role="alert" className="text-[12px] text-dismiss">
          {error}
        </p>
      )}

      <MemberRoster adapter={adapter} canManage={canManage} />

      {modalOpen && <InviteModal create={create} onClose={() => setModalOpen(false)} />}
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
