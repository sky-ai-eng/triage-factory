import { useEffect, useRef, useState } from 'react'
import { ChevronDown, Check } from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import { useOrgContext } from '../contexts/OrgContext'
import { useSwitchOrg } from '../hooks/useSwitchOrg'

/**
 * Topbar dropdown for switching between orgs the user belongs to.
 * Mounted only in multi mode (Shell decides). Picking an org moves the
 * session there through useSwitchOrg, which also rewrites the URL so deep
 * links to the same sub-path inside the new org work.
 *
 * An org that requires an SSO this session did not come through is listed
 * with that org's sign-in instead: switching to it would only be refused, so
 * the row is a link to its start URL and never calls /api/me/active-org.
 */
export default function OrgPicker() {
  const auth = useAuth()
  const { activeOrgId } = useOrgContext()
  const switchOrg = useSwitchOrg()
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
    await switchOrg(newOrgId)
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 text-body font-medium px-3 py-1.5 rounded-full text-ink-2 hover:text-ink-1 hover:bg-tint-2 transition-colors"
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className="truncate max-w-[160px]">{active.name}</span>
        <ChevronDown size={14} className="text-ink-3" />
      </button>

      {open && (
        <div
          role="listbox"
          className="absolute left-0 top-full mt-1.5 min-w-[200px] backdrop-blur-xl bg-raised border border-line-1 rounded-xl shadow-float shadow-black/[0.08] py-1 z-50"
        >
          {auth.orgs.map((org) => {
            const isActive = org.id === active.id
            if (org.sso_required && org.sso_start_url) {
              return (
                <a
                  key={org.id}
                  href={org.sso_start_url}
                  className="w-full flex items-center justify-between gap-3 px-3 py-2 text-left text-body text-ink-1 hover:bg-tint-2 transition-colors"
                >
                  <span className="flex flex-col">
                    <span className="font-medium">{org.name}</span>
                    <span className="text-reported text-warm">Sign in with single sign-on</span>
                  </span>
                  {isActive && <Check size={14} className="text-warm shrink-0" />}
                </a>
              )
            }
            return (
              <button
                key={org.id}
                type="button"
                role="option"
                aria-selected={isActive}
                onClick={() => handlePick(org.id)}
                className="w-full flex items-center justify-between gap-3 px-3 py-2 text-left text-body text-ink-1 hover:bg-tint-2 transition-colors"
              >
                <span className="flex flex-col">
                  <span className="font-medium">{org.name}</span>
                  <span className="text-reported text-ink-3 capitalize">{org.role}</span>
                </span>
                {isActive && <Check size={14} className="text-warm shrink-0" />}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
