import { useCallback, useEffect, useRef, useState } from 'react'
import { httpErrorMessage } from '../lib/apiClient'
import {
  createApiToken,
  listApiTokens,
  readApiTokenPolicy,
  renameApiToken,
  revokeApiToken,
} from '../lib/apiTokens'
import type { ApiToken, ApiTokenCreateRequest, ApiTokenCreated, AuthOrg } from '../types'

// useTokens owns the personal-settings token table: the caller's tokens across
// every org, the cap each of their orgs sets, and the three mutations. The
// mutations keep the held list honest without a re-read where the answer is
// already known — a create prepends the row the server returned, a rename
// replaces it, a revoke drops it — and `reload` is there for the rest.

export interface UseTokens {
  tokens: ApiToken[]
  /** Each membership's cap, keyed by org id; null = no cap. An org whose read
   *  failed is absent, which the page treats as "cap unknown" rather than
   *  "no cap" — a struck preset is a promise about the 422. */
  caps: Record<string, number | null>
  loading: boolean
  /** The last failure's message, or '' — the list is left as it was. */
  error: string
  create: (body: ApiTokenCreateRequest) => Promise<ApiTokenCreated>
  revoke: (id: string, opts?: { keepalive?: boolean }) => Promise<void>
  rename: (id: string, name: string) => Promise<ApiToken>
  reload: () => void
}

export function useTokens(enabled: boolean, orgs: AuthOrg[]): UseTokens {
  const [tokens, setTokens] = useState<ApiToken[]>([])
  const [caps, setCaps] = useState<Record<string, number | null>>({})
  const [loading, setLoading] = useState(enabled)
  const [error, setError] = useState('')
  const [gen, setGen] = useState(0)
  // Memberships change identity on every /api/me refresh; the ids are what
  // the policy reads depend on.
  const orgKey = orgs.map((o) => o.id).join(',')
  // Every mutation bumps this; a list read that began before a mutation is
  // stale by the time it lands, and applying it would undo what the
  // mutation already showed. Such a read re-reads instead.
  const mutSeq = useRef(0)

  useEffect(() => {
    if (!enabled) return
    const ctrl = new AbortController()
    const ids = orgKey ? orgKey.split(',') : []
    setLoading(true)
    const seqAtStart = mutSeq.current
    void (async () => {
      try {
        const [rows, policies] = await Promise.all([
          listApiTokens(),
          Promise.all(
            ids.map((id) =>
              readApiTokenPolicy(id).then(
                (p) => [id, p.max_age_days] as const,
                () => null,
              ),
            ),
          ),
        ])
        if (ctrl.signal.aborted) return
        if (mutSeq.current !== seqAtStart) {
          setGen((g) => g + 1)
          return
        }
        setTokens(rows)
        const next: Record<string, number | null> = {}
        for (const p of policies) if (p) next[p[0]] = p[1]
        setCaps(next)
        setError('')
      } catch (err) {
        if (ctrl.signal.aborted) return
        setError(httpErrorMessage(err, 'Could not load your API tokens.'))
      } finally {
        if (!ctrl.signal.aborted) setLoading(false)
      }
    })()
    return () => ctrl.abort()
  }, [enabled, orgKey, gen])

  const create = useCallback(async (body: ApiTokenCreateRequest) => {
    const made = await createApiToken(body)
    mutSeq.current++
    // The list is newest-first; the row is what the server returned, minus
    // the secret the list never carries.
    const { token: _secret, ...row } = made
    void _secret
    setTokens((t) => [row, ...t])
    return made
  }, [])

  const revoke = useCallback(async (id: string, opts?: { keepalive?: boolean }) => {
    await revokeApiToken(id, opts)
    mutSeq.current++
    setTokens((t) => t.filter((r) => r.id !== id))
  }, [])

  const rename = useCallback(async (id: string, name: string) => {
    const row = await renameApiToken(id, name)
    mutSeq.current++
    setTokens((t) => t.map((r) => (r.id === id ? row : r)))
    return row
  }, [])

  const reload = useCallback(() => setGen((g) => g + 1), [])

  return { tokens, caps, loading, error, create, revoke, rename, reload }
}
