import { useCallback, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { HttpError } from '../lib/apiClient'

// RosterMember is the normalized row <MemberRoster> renders, independent of
// which tier (org or team) the adapter fetched it from. githubUsername /
// jiraAccountId are null when the member holds no host-scoped identity
// binding — the "Not connected" readiness state.
export interface RosterMember {
  userId: string
  displayName: string
  githubUsername: string | null
  jiraAccountId: string | null
  role: string
  isCurrentUser: boolean
}

// MemberRosterAdapter is the seam between the shared roster UI and a specific
// membership tier. The org adapter (TFAC-417) wires the /api/orgs/{org}/members
// endpoints; the team adapter (TFAC-415) will wire the team endpoints and fill
// the extra slots. The four divergence points called out in the epic:
//
//  1. roles / protectedRole — the role vocabulary + the role whose last holder
//     can't be demoted/removed (org: owner | admin | member, protected=owner).
//  2. addAffordance — the "add member" control (org: invite-by-email, filled
//     by the frontend-invite ticket; team: an org-member picker).
//  3. protectedRole — the last-privileged guard constant (see above).
//  4. extraRows — extra non-member rows (org: none; team: the bot row).
//
// fetchMembers / changeRole / remove own the I/O; the headless hook below owns
// the list + loading + error + the mutation choreography.
export interface MemberRosterAdapter {
  roles: string[]
  protectedRole: string
  // roleLabels optionally overrides how a role value renders in the role
  // <select>; the value sent to the backend stays the raw role. The team
  // adapter uses it to spell out that 'viewer' is a read-only role (enforced
  // end-to-end as of TFAC-447). The org adapter leaves it undefined and the
  // select shows the raw role.
  roleLabels?: Record<string, string>
  fetchMembers(): Promise<RosterMember[]>
  changeRole(userId: string, role: string): Promise<void>
  remove(userId: string): Promise<void>
  // transferOwnership, when present, enables the owner-only "Transfer
  // ownership" flow (TFAC-420): hand the protected role to another member.
  // The org adapter wires it to POST .../transfer-ownership; the team adapter
  // leaves it undefined (teams have no ownership-transfer concept), so the
  // roster shows no transfer affordance there. Resolves on 204; throws
  // HttpError on the backend's 403/409/422 so the picker can surface it.
  transferOwnership?(newOwnerUserId: string): Promise<void>
  addAffordance?: ReactNode
  extraRows?: ReactNode
}

export interface MemberRosterState {
  members: RosterMember[]
  loading: boolean
  error: string | null
  // userId of the row whose mutation is in flight (disables its controls);
  // null when idle. One mutation at a time keeps the list a stable target.
  pendingId: string | null
  // Inline error from the last role-change / remove — notably the §1 409 the
  // backend returns for the last-owner guard, surfaced verbatim.
  actionError: string | null
  changeRole(userId: string, role: string): void
  remove(userId: string): void
  reload(): void
  clearActionError(): void
}

// useMemberRoster owns the roster's data lifecycle for a given adapter. The
// adapter MUST be referentially stable across renders (wrap it in useMemo) —
// the load/mutation callbacks depend on its methods, so a fresh object each
// render would re-fetch in a loop.
export function useMemberRoster(adapter: MemberRosterAdapter): MemberRosterState {
  const { fetchMembers, changeRole: adapterChangeRole, remove: adapterRemove } = adapter

  const [members, setMembers] = useState<RosterMember[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [pendingId, setPendingId] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      setMembers(await fetchMembers())
    } catch (e) {
      setError(messageFrom(e, 'Could not load members.'))
    } finally {
      setLoading(false)
    }
  }, [fetchMembers])

  // Fetch on mount and whenever the adapter (hence fetchMembers) changes —
  // e.g. switching the active org swaps the org adapter.
  useEffect(() => {
    void load()
  }, [load])

  // runMutation centralizes the optimistic-disable → call → refetch → surface-
  // error choreography both mutations share. On success it reloads so the row
  // reflects committed state (a role flip, or the member vanishing); on failure
  // it leaves the list untouched and shows the message inline.
  const runMutation = useCallback(
    async (userId: string, op: () => Promise<void>) => {
      setPendingId(userId)
      setActionError(null)
      try {
        await op()
        await load()
      } catch (e) {
        setActionError(messageFrom(e, 'That change could not be applied.'))
      } finally {
        setPendingId(null)
      }
    },
    [load],
  )

  const changeRole = useCallback(
    (userId: string, role: string) => {
      void runMutation(userId, () => adapterChangeRole(userId, role))
    },
    [runMutation, adapterChangeRole],
  )

  const remove = useCallback(
    (userId: string) => {
      void runMutation(userId, () => adapterRemove(userId))
    },
    [runMutation, adapterRemove],
  )

  // reload re-fetches the roster. Memoized so its identity is stable across
  // renders (load only changes when the adapter's fetchMembers does), letting
  // consumers use it as an effect / useCallback dependency without churn.
  const reload = useCallback(() => {
    void load()
  }, [load])

  return {
    members,
    loading,
    error,
    pendingId,
    actionError,
    changeRole,
    remove,
    reload,
    clearActionError: () => setActionError(null),
  }
}

// displayNameFor renders a roster row's name: the captured display_name, else
// "@<githubUsername>" when a GitHub login was captured but the profile had no
// "Name" set (TFAC-560), else the literal "no identity at all" fallback.
export function displayNameFor(
  member: Pick<RosterMember, 'displayName' | 'githubUsername'>,
): string {
  if (member.displayName) return member.displayName
  return member.githubUsername ? `@${member.githubUsername}` : 'Unnamed user'
}

// initialFor derives the avatar-badge letter from the SAME source
// displayNameFor renders, so the initial never shows "?" while the name next
// to it reads "@octocat" (TFAC-560) — it reads the raw githubUsername (not
// the "@"-prefixed display text) so the initial is a letter, not "@".
export function initialFor(member: Pick<RosterMember, 'displayName' | 'githubUsername'>): string {
  const source = member.displayName || member.githubUsername || 'Unnamed user'
  return (source.trim()[0] ?? '?').toUpperCase()
}

// messageFrom prefers the server's JSON { error } message (so the last-owner
// 409's friendly text reaches the user verbatim) and falls back to the error's
// own message, then a generic line. Exported so the transfer-ownership picker
// surfaces the same backend errors (403/409/422) the inline roster mutations do.
export function messageFrom(e: unknown, fallback: string): string {
  if (e instanceof HttpError) {
    try {
      const body = JSON.parse(e.body) as { error?: unknown }
      if (typeof body.error === 'string' && body.error) return body.error
    } catch {
      // body wasn't JSON — fall through to the generic handling below.
    }
  }
  return e instanceof Error ? e.message : fallback
}
