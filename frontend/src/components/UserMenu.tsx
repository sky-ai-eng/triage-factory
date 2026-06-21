import { useEffect, useRef, useState } from 'react'
import { LogOut, User as UserIcon } from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import LoginMethods from './LoginMethods'

/**
 * Avatar + popover in the topbar. Multi-mode only. Provides the
 * logout action — the rest of the menu is intentionally sparse until
 * D14 lands the proper account/admin surfaces.
 */
export default function UserMenu() {
  const auth = useAuth()
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

  if (!auth.me) return null

  const initial = (
    auth.me.display_name?.[0] ||
    auth.me.github_username?.[0] ||
    auth.me.email?.[0] ||
    '?'
  ).toUpperCase()

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="w-8 h-8 rounded-full bg-accent-soft text-accent flex items-center justify-center text-[12px] font-semibold hover:ring-2 hover:ring-accent/20 transition-all overflow-hidden"
        aria-haspopup="menu"
        aria-expanded={open}
        title={auth.me.display_name || auth.me.email}
      >
        {auth.me.avatar_url ? (
          <img src={auth.me.avatar_url} alt="" className="w-full h-full object-cover" />
        ) : (
          initial
        )}
      </button>

      {open && (
        <div
          role="menu"
          className="absolute right-0 top-full mt-1.5 min-w-[220px] backdrop-blur-xl bg-surface-raised border border-border-glass rounded-xl shadow-lg shadow-black/[0.08] py-1 z-50"
        >
          <div className="px-3 py-2 border-b border-border-subtle">
            <div className="text-[13px] font-medium text-text-primary truncate">
              {auth.me.display_name || auth.me.github_username || 'Account'}
            </div>
            {auth.me.email && (
              <div className="text-[11px] text-text-tertiary truncate">{auth.me.email}</div>
            )}
          </div>
          {/* Linked login identities (the principal's GitHub + N SSO logins).
              Fetches lazily — only mounts while the menu is open. */}
          <LoginMethods />
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false)
              void auth.logout()
            }}
            className="w-full flex items-center gap-2 px-3 py-2 text-left text-[13px] text-text-primary hover:bg-black/[0.03] transition-colors"
          >
            <LogOut size={14} className="text-text-tertiary" />
            Log out
          </button>
          <div className="px-3 py-1 text-[10px] text-text-tertiary uppercase tracking-wide flex items-center gap-1">
            <UserIcon size={10} />
            <span>signed in</span>
          </div>
        </div>
      )}
    </div>
  )
}
