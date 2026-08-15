import { useEffect, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router'
import { ChevronDown, Check } from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import { useOrgContext } from '../contexts/OrgContext'
import { reconnectWebSocket } from '../hooks/useWebSocket'
import { invalidateTeams } from '../hooks/useTeams'
import { invalidateEntitlements } from '../hooks/useEntitlements'
import { apiFetch } from '../lib/apiClient'

/**
 * Topbar dropdown for switching between orgs the user belongs to.
 * Mounted only in multi mode (Shell decides). When the user picks a
 * new org, we both update OrgContext and rewrite the URL so deep
 * links to the same sub-path inside the new org work — e.g. switching
 * from /orgs/A/triage to /orgs/B/triage preserves the /triage tail.
 */
export default function OrgPicker() {
  const auth = useAuth()
  const { activeOrgId, setActiveOrgId } = useOrgContext()
  const navigate = useNavigate()
  const location = useLocation()
  const [open, setOpen] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    function onClick(e: MouseEvent) {
      if (!rootRef.current?.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onClick)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onClick)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  if (auth.orgs.length === 0) return null

  const active = auth.orgs.find((o) => o.id === activeOrgId) ?? auth.orgs[0]

  const handlePick = async (newOrgId: string) => {
    setOpen(false)
    if (newOrgId === activeOrgId) return
    setActiveOrgId(newOrgId)

    // Persist the choice on the session row so the server's
    // withSession picks up the new active org for every subsequent
    // request (and so cross-tab /api/me reads agree). Fire-and-log
    // failures: the local pick already updated OrgContext, and the
    // next /api/me will reconcile a transient persistence failure on
    // the user's next interaction.
    try {
      await apiFetch('/api/me/active-org', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ org_id: newOrgId }),
      })
      // Drop the cached /api/teams response only AFTER the active org
      // is persisted server-side. /api/teams is session-org-scoped, so
      // invalidating before the POST commits would race and refetch the
      // OLD org's teams; doing it here guarantees the reload reads the
      // new org's set. (Codex review on PR #263.)
      invalidateTeams()
      // Entitlements are scoped to the active org too (multi mode), so drop
      // their cache on the same commit — the new org may license a different
      // EE feature set. Same post-persist ordering rationale as teams.
      invalidateEntitlements()
      // Force the WS to re-handshake so the hub's per-(user, org)
      // Broadcast filter routes events for the just-switched
      // tenant rather than the previous one. Without this the
      // user keeps receiving the old org's events until the next
      // unrelated disconnect.
      reconnectWebSocket()
    } catch (err) {
      // A refusal (revoked membership) or a network drop both land here. The
      // picker rolling back the local selection would conflict with whatever
      // URL navigation is about to happen below; for now log and let the
      // AuthGate / /api/me reconciliation correct it on the next page render.
      console.warn('[org-picker] active-org POST failed:', err)
    }

    // Rewrite the URL: swap the org_id segment, preserve everything
    // after it. Routes that don't start with /orgs/<id> fall back to
    // the new org's root.
    const swapped = location.pathname.match(/^\/orgs\/[^/]+(\/.*)?$/)
    const tail = swapped?.[1] ?? ''
    navigate('/orgs/' + newOrgId + tail, { replace: false })
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 text-[13px] font-medium px-3 py-1.5 rounded-full text-text-secondary hover:text-text-primary hover:bg-black/[0.03] transition-colors"
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className="truncate max-w-[160px]">{active.name}</span>
        <ChevronDown size={14} className="text-text-tertiary" />
      </button>

      {open && (
        <div
          role="listbox"
          className="absolute left-0 top-full mt-1.5 min-w-[200px] backdrop-blur-xl bg-surface-raised border border-border-glass rounded-xl shadow-lg shadow-black/[0.08] py-1 z-50"
        >
          {auth.orgs.map((org) => {
            const isActive = org.id === active.id
            return (
              <button
                key={org.id}
                type="button"
                role="option"
                aria-selected={isActive}
                onClick={() => handlePick(org.id)}
                className="w-full flex items-center justify-between gap-3 px-3 py-2 text-left text-[13px] text-text-primary hover:bg-black/[0.03] transition-colors"
              >
                <span className="flex flex-col">
                  <span className="font-medium">{org.name}</span>
                  <span className="text-[11px] text-text-tertiary capitalize">{org.role}</span>
                </span>
                {isActive && <Check size={14} className="text-accent shrink-0" />}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
