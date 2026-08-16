import { useCallback, useEffect } from 'react'
import { apiFetch, apiJSON } from '../lib/apiClient'
import { usePagedList } from './usePagedList'
import type { CreateInviteResponse, PendingInvite } from '../types'

// useInvites owns the org People tab's pending-invite list + the create /
// revoke / resend mutations. Multi-mode org-admin surface only: the backend
// 403s the list for a non-admin and 404s it in local mode, so the hook is
// gated on `enabled` (the caller's canManage) and stays inert — empty list, no
// fetch — when the viewer can't manage the org. Both statuses render as an
// empty panel rather than an error, which keeps a role change mid-session from
// surfacing a scary line.
//
// All calls hit the literal /api/invites paths WITHOUT apiClient's `org`
// option: the routes are session-org-scoped (the backend reads the active org
// from the session, like /api/teams), so an `org`-prefixed path would 404.
export interface UseInvites {
  invites: PendingInvite[]
  /** Outstanding invites across every page, for a count badge. */
  total: number | null
  loading: boolean
  error: string | null
  /** True when the server minted a token for a further page. */
  hasMore: boolean
  loadMore: () => Promise<void>
  /** Create a fresh invite. Resolves to the one-time accept link; throws
   *  HttpError (notably the 409 "already pending") for the caller to surface. */
  create: (email: string, role: string, targetTeamId: string) => Promise<CreateInviteResponse>
  /** Revoke a pending invite by id. */
  revoke: (id: string) => Promise<void>
  /** Rotate an invite's token: the backend 409s a second live pending invite
   *  for the same address (unique index), so "resend" = revoke the old token
   *  then mint a fresh one. Resolves to the new accept link. */
  resend: (invite: PendingInvite) => Promise<CreateInviteResponse>
  reload: () => void
}

// Hoisted so the option object is referentially stable across renders.
const INVITES_EMPTY_ON = { emptyOnStatus: [403, 404] }

export function useInvites(enabled: boolean): UseInvites {
  const page = usePagedList<PendingInvite>(
    '/api/invites/list',
    'Could not load invites.',
    INVITES_EMPTY_ON,
  )
  const { load: loadPage, setItems } = page

  const load = useCallback(async () => {
    if (!enabled) {
      setItems(() => [])
      return
    }
    await loadPage({})
  }, [enabled, loadPage, setItems])

  useEffect(() => {
    void load()
  }, [load])

  const create = useCallback(
    async (email: string, role: string, targetTeamId: string): Promise<CreateInviteResponse> => {
      const res = await apiJSON<CreateInviteResponse>('/api/invites', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, role, target_team_id: targetTeamId }),
      })
      await load()
      return res
    },
    [load],
  )

  const revoke = useCallback(
    async (id: string): Promise<void> => {
      await apiFetch(`/api/invites/${id}/revoke`, { method: 'POST' })
      await load()
    },
    [load],
  )

  const resend = useCallback(
    async (invite: PendingInvite): Promise<CreateInviteResponse> => {
      // Revoke the live invite first to free the (org, email) active-unique
      // index, then create afresh — the new row gets a NEW id, since the old
      // one is now revoked and drops out of the active list.
      //
      // load() is in `finally` so the list ALWAYS reconciles, even on a partial
      // failure: if the revoke succeeds but the create then throws (a network
      // blip), the invite is already gone server-side. Without the reload the UI
      // would keep rendering a stale "pending" ghost row, and a retry would 404
      // the already-revoked id. Reloading drops the ghost row so the admin can
      // re-invite cleanly. load() captures its own errors (never throws), so it
      // can't mask the original failure that propagates to the caller.
      try {
        await apiFetch(`/api/invites/${invite.id}/revoke`, { method: 'POST' })
        return await apiJSON<CreateInviteResponse>('/api/invites', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            email: invite.email,
            role: invite.role,
            target_team_id: invite.target_team_id ?? '',
          }),
        })
      } finally {
        await load()
      }
    },
    [load],
  )

  return {
    invites: page.items,
    total: page.total,
    loading: page.loading,
    error: page.error === '' ? null : page.error,
    hasMore: page.hasMore,
    loadMore: page.loadMore,
    create,
    revoke,
    resend,
    reload: () => void load(),
  }
}
