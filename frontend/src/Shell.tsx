import { NavLink, Outlet, useLocation } from 'react-router-dom'
import { Settings } from 'lucide-react'
import { useOrgHref } from './hooks/useOrgHref'
import { useOptionalAuth } from './contexts/AuthContext'
import { useTemplateScope } from './hooks/useTemplateScope'
import { useOrgRole } from './hooks/useOrgRole'
import OrgPicker from './components/OrgPicker'
import UserMenu from './components/UserMenu'

const navItems = [
  { to: '/', label: 'Factory' },
  { to: '/triage', label: 'Triage' },
  { to: '/board', label: 'Board' },
  { to: '/prs', label: 'PRs' },
  { to: '/projects', label: 'Projects' },
  { to: '/repos', label: 'Repos' },
  { to: '/prompts', label: 'Prompts' },
  { to: '/brief', label: 'Brief' },
]

export default function Shell() {
  const orgHref = useOrgHref()
  const location = useLocation()
  // useOptionalAuth returns the multi-mode auth context if present,
  // null in local mode. The org picker and user menu are multi-only;
  // local mode hides them.
  const auth = useOptionalAuth()
  const isMulti = auth !== null
  // The org-template editor (SKY-381) is an org-admin surface; its nav entry
  // renders only for owners/admins in multi mode (useTemplateScope is false in
  // local mode). It is deliberately a distinct entry — never the TeamSwitch.
  const { available: templateAvailable } = useTemplateScope()
  // The /org surface is admin-gated in the nav (owner/admin of the active
  // org). Non-admins can still deep-link to it for a read-only roster + Leave,
  // but it doesn't earn a nav slot for them. False in local mode (no auth).
  const { isAdmin: orgAdmin } = useOrgRole()

  // Full-bleed routes (the agent run station) drop the app nav + main padding so
  // the page owns the whole viewport — it's a focused, open-in-new-tab surface.
  const fullBleed = /\/runs\/[^/]+\/?$/.test(location.pathname)

  return (
    <div className="min-h-screen bg-surface text-text-primary">
      {!fullBleed && (
        <nav className="sticky top-0 z-50 backdrop-blur-xl bg-surface-overlay border-b border-border-subtle px-8 py-4 flex items-center gap-6">
          <span className="text-base font-semibold tracking-tight text-text-primary">
            Triage Factory
          </span>
          {isMulti && <OrgPicker />}
          <div className="flex gap-1 flex-1">
            {navItems.map((item) => (
              <NavLink
                key={item.to}
                to={orgHref(item.to)}
                end={item.to === '/'}
                className={({ isActive }) =>
                  `text-[13px] font-medium px-4 py-1.5 rounded-full transition-all duration-200 ${
                    isActive
                      ? 'bg-accent-soft text-accent'
                      : 'text-text-tertiary hover:text-text-secondary hover:bg-black/[0.03]'
                  }`
                }
              >
                {item.label}
              </NavLink>
            ))}
            {orgAdmin && (
              <NavLink
                to={orgHref('/org')}
                className={({ isActive }) =>
                  `text-[13px] font-medium px-4 py-1.5 rounded-full transition-all duration-200 ${
                    isActive
                      ? 'bg-accent-soft text-accent'
                      : 'text-text-tertiary hover:text-text-secondary hover:bg-black/[0.03]'
                  }`
                }
              >
                Org
              </NavLink>
            )}
            {templateAvailable && (
              <NavLink
                to={orgHref('/org-template')}
                className={({ isActive }) =>
                  `text-[13px] font-medium px-4 py-1.5 rounded-full transition-all duration-200 ${
                    isActive
                      ? 'bg-accent-soft text-accent'
                      : 'text-text-tertiary hover:text-text-secondary hover:bg-black/[0.03]'
                  }`
                }
              >
                Org template
              </NavLink>
            )}
          </div>
          <NavLink
            to={orgHref('/settings')}
            className={({ isActive }) =>
              `p-2 rounded-full transition-all duration-200 ${
                isActive
                  ? 'bg-accent-soft text-accent'
                  : 'text-text-tertiary hover:text-text-secondary hover:bg-black/[0.03]'
              }`
            }
          >
            <Settings size={16} />
          </NavLink>
          {isMulti && <UserMenu />}
        </nav>
      )}
      <main className={fullBleed ? '' : 'px-8 py-8'}>
        <Outlet />
      </main>
    </div>
  )
}
