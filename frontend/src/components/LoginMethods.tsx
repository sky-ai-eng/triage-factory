import { useEffect, useState } from 'react'
import { Check } from 'lucide-react'
import { apiJSON } from '../lib/apiClient'
import type { IdentitiesResponse, LoginMethod } from '../types'

/**
 * LoginMethods — the read-only "Login methods" section of the account
 * surface. Lists the login identities linked to the signed-in principal
 * (a GitHub login + N SSO providers, all resolving to one account via
 * verified-email linking) with a verified badge, the linked-on date, and
 * a marker on the identity backing the current session.
 *
 * Self-contained and mode-agnostic: it fetches GET /api/me/identities
 * (a personal read that works in both modes) and renders whatever rows
 * come back, so it needs no knowledge of local vs multi. Mounted in the
 * multi-mode UserMenu, where the cross-provider-link / SSO-enforcement
 * story (TFAC-432) is load-bearing.
 */

function providerLabel(provider: string): string {
  switch (provider) {
    case 'github':
      return 'GitHub'
    case 'saml':
      return 'SSO'
    case 'local':
      return 'Local'
    default:
      return provider
  }
}

function linkedOn(iso?: string): string | null {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

type State =
  | { status: 'loading' }
  | { status: 'error' }
  | { status: 'ready'; methods: LoginMethod[] }

export default function LoginMethods() {
  const [state, setState] = useState<State>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false
    apiJSON<IdentitiesResponse>('/api/me/identities')
      .then((d) => {
        if (!cancelled) setState({ status: 'ready', methods: d.methods ?? [] })
      })
      .catch(() => {
        if (!cancelled) setState({ status: 'error' })
      })
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="px-3 py-2 border-b border-border-subtle">
      <div className="mb-1.5 text-[10px] font-medium uppercase tracking-wide text-text-tertiary">
        Login methods
      </div>

      {state.status === 'loading' && (
        <div className="py-0.5 text-[11px] text-text-tertiary">Loading…</div>
      )}
      {state.status === 'error' && (
        <div className="py-0.5 text-[11px] text-text-tertiary">Couldn’t load login methods.</div>
      )}
      {state.status === 'ready' && state.methods.length === 0 && (
        <div className="py-0.5 text-[11px] text-text-tertiary">No linked login methods.</div>
      )}

      {state.status === 'ready' && state.methods.length > 0 && (
        <ul className="space-y-1.5">
          {state.methods.map((m, i) => {
            const date = linkedOn(m.linked_at)
            return (
              <li key={`${m.provider}-${m.email ?? ''}-${i}`} className="leading-tight">
                <div className="flex items-center gap-1.5">
                  <span className="text-[12px] font-medium text-text-primary">
                    {providerLabel(m.provider)}
                  </span>
                  {m.email_verified && (
                    <span
                      className="inline-flex items-center gap-0.5 text-[10px] text-claim"
                      title="Verified email"
                    >
                      <Check size={10} aria-hidden />
                      verified
                    </span>
                  )}
                  {m.current && (
                    <span
                      className="text-[10px] text-accent"
                      title="The login method backing your current session"
                    >
                      · current
                    </span>
                  )}
                </div>
                {m.email && (
                  <div className="truncate text-[11px] text-text-tertiary">{m.email}</div>
                )}
                {date && <div className="text-[10px] text-text-tertiary">Linked {date}</div>}
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
