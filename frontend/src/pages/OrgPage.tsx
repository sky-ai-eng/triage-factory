import { useCallback, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router'
import { Plus, Users } from 'lucide-react'
import * as Switch from '@radix-ui/react-switch'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import { useInvites } from '../hooks/useInvites'
import MemberRoster from '../components/MemberRoster'
import InviteModal from '../components/InviteModal'
import PendingInviteRow from '../components/PendingInviteRow'
import OrgSettings from './settings/stack/OrgSettings'
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
        const data = await apiJSON<{ members: OrgMemberApiRow[] }>(
          `/api/orgs/${encodeURIComponent(orgId)}/members`,
        )
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
        await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/members/${userId}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ role }),
        })
      },
      async remove(userId) {
        await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/members/${userId}`, {
          method: 'DELETE',
        })
      },
      async transferOwnership(newOwnerUserId) {
        // Owner-only on the backend (gated on tf.user_owns_org + the
        // guard_org_owner_transfer trigger). 204 on success; apiFetch throws
        // HttpError on 403/409/422 so the picker surfaces the friendly message.
        await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/transfer-ownership`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ new_owner_user_id: newOwnerUserId }),
        })
      },
    }),
    [orgId],
  )
}

type OrgTab = 'people' | 'settings'

const TABS: { id: OrgTab; label: string }[] = [
  { id: 'people', label: 'People' },
  { id: 'settings', label: 'Settings' },
]

// OrgPage is the multi-mode org surface: the [People · Settings] shell.
// People is the shared roster + invite surface (TFAC-417/418); Settings hosts
// the relocated OrgSettings surface (TFAC-419) — the org-scoped config moved
// off the global Settings page. The active tab is URL-driven so the surfaces
// stay deep-linkable.
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
// People tab's adapter always gets a concrete orgId. The active tab is
// URL-driven (?tab=) so the relocated Settings surface is deep-linkable and
// survives a refresh.
function OrgPageBody({ orgId }: { orgId: string }) {
  const { isAdmin } = useOrgRole()
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = resolveTab(searchParams.get('tab'), isAdmin)
  // People owns the bare URL (drop the param); the admin tabs carry an explicit
  // ?tab=. replace so flipping tabs doesn't pile up history entries.
  const setTab = (t: OrgTab) => setSearchParams(t === 'people' ? {} : { tab: t }, { replace: true })

  // People (the roster) is everyone's; Settings is org-admin. Non-admins see
  // just People. /org is multi-mode only (no local route), so OrgSettings
  // always renders multi (isLocal=false).
  const tabs = isAdmin ? TABS : TABS.filter((t) => t.id === 'people')

  return (
    <div className="mx-auto max-w-3xl">
      <div className="mb-5 flex shrink-0 items-center gap-2.5">
        <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent-soft text-accent">
          <Users size={15} />
        </span>
        <div>
          <h1 className="text-[17px] font-semibold leading-tight text-text-primary">
            Organization
          </h1>
          <p className="text-[11px] leading-tight text-text-tertiary">People and settings.</p>
        </div>
      </div>

      <div className="mb-5 flex shrink-0 gap-1 border-b border-border-subtle">
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

      {tab === 'people' && <OrgPeople orgId={orgId} canManage={isAdmin} />}
      {tab === 'settings' && <OrgSettings orgId={orgId} isLocal={false} />}
    </div>
  )
}

// resolveTab maps the ?tab= param to a concrete tab, gating Settings on org
// admin so a stale or hand-typed ?tab= can't surface a tab the viewer can't
// use. People is the default + floor — belt-and-suspenders to the tab-strip
// filter.
function resolveTab(raw: string | null, isAdmin: boolean): OrgTab {
  if (raw === 'settings' && isAdmin) return 'settings'
  return 'people'
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
              // Radix renders a <button role="switch">, and a wrapping <label>
              // doesn't reliably name a button — so name it explicitly.
              aria-label="Show invited"
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
