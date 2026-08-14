import { Link, NavLink, Outlet, useLocation } from 'react-router'
import { Settings } from 'lucide-react'
import { useOrgHref } from './hooks/useOrgHref'
import { useOptionalAuth } from './contexts/AuthContext'
import { useOrgRole } from './hooks/useOrgRole'
import { FeatureFleet, useEntitlements } from './hooks/useEntitlements'
import OrgPicker from './components/OrgPicker'
import UserMenu from './components/UserMenu'

// The base nav shared by both modes. Prompts + Team are mode-dependent (the
// /team surface is multi-only), so they're spliced in per-mode below.
const baseNavLeading = [
  { to: '/', label: 'Factory' },
  { to: '/triage', label: 'Triage' },
  { to: '/board', label: 'Board' },
  { to: '/prs', label: 'PRs' },
  { to: '/projects', label: 'Projects' },
  { to: '/repos', label: 'Repos' },
  // Usage — the spend dashboard (TFAC-479). Everyone gets at least the
  // personal view; the team/org sections inside the page self-gate on role.
  { to: '/usage', label: 'Usage' },
]

export default function Shell() {
  const orgHref = useOrgHref()
  const location = useLocation()
  // useOptionalAuth returns the multi-mode auth context if present,
  // null in local mode. The org picker and user menu are multi-only;
  // local mode hides them.
  const auth = useOptionalAuth()
  const isMulti = auth !== null
  // The /org surface is admin-gated in the nav (owner/admin of the active
  // org). Non-admins can still deep-link to it for a read-only roster + Leave,
  // but it doesn't earn a nav slot for them. False in local mode (no auth).
  const { isAdmin: orgAdmin } = useOrgRole()

  // Fleet console (TFAC-589) — the sandbox-fleet admin surface. Composes the two
  // deployment-scoped gates: the operator identity (from /api/me, org-less) AND
  // the FeatureFleet entitlement. Both must hold; absent otherwise (no upsell
  // stub), matching the backend's 404-and-hide. Local mode is unlicensed, so the
  // item never shows there even though the single user is nominally an operator.
  const { has, loaded: entLoaded } = useEntitlements()
  const fleetVisible = entLoaded && has(FeatureFleet) && (auth?.me?.is_operator ?? false)

  // Full-bleed routes (the agent run station) drop the app nav + main padding so
  // the page owns the whole viewport — it's a focused, open-in-new-tab surface.
  const fullBleed = /\/runs\/[^/]+\/?$/.test(location.pathname)

  // The shared pill style for every nav entry. Factored out so the mode-specific
  // Team/Prompts entries (whose active state we compute by hand, since they
  // share the /team pathname and differ only by ?tab) match the NavLink ones.
  const pill = (isActive: boolean) =>
    `text-body font-medium px-4 py-1.5 rounded-full transition-all duration-200 ${
      isActive
        ? 'bg-warm-2 text-warm'
        : 'text-ink-3 hover:text-ink-2 hover:bg-tint-2'
    }`

  // Team + Prompts both live on /team in multi mode (Prompts is the ?tab=prompts
  // deep-link), so NavLink's pathname-only matching can't tell them apart —
  // compute their active state from the query here.
  const onTeam = /\/team\/?$/.test(location.pathname)
  const promptsTabActive = onTeam && new URLSearchParams(location.search).get('tab') === 'prompts'
  const teamHomeActive = onTeam && !promptsTabActive

  return (
    <div className="min-h-screen bg-ground text-ink-1">
      {!fullBleed && (
        <nav className="sticky top-0 z-50 backdrop-blur-xl bg-raised border-b border-line-1 px-8 py-4 flex items-center gap-6">
          <span className="text-base font-semibold tracking-tight text-ink-1">
            Triage Factory
          </span>
          {isMulti && <OrgPicker />}
          <div className="flex gap-1 flex-1">
            {baseNavLeading.map((item) => (
              <NavLink
                key={item.to}
                to={orgHref(item.to)}
                end={item.to === '/'}
                className={({ isActive }) => pill(isActive)}
              >
                {item.label}
              </NavLink>
            ))}
            {/* Team — multi-mode only (the /team surface has no local route).
                Defaults to the Members tab. */}
            {isMulti && (
              <Link
                to={orgHref('/team')}
                aria-current={teamHomeActive ? 'page' : undefined}
                className={pill(teamHomeActive)}
              >
                Team
              </Link>
            )}
            {/* Prompts — one-click reach to the binding-graph canvas. In multi
                mode it deep-links straight to the /team Prompts tab; in local
                mode it's the standalone /prompts page. */}
            {isMulti ? (
              <Link
                to={orgHref('/team') + '?tab=prompts'}
                aria-current={promptsTabActive ? 'page' : undefined}
                className={pill(promptsTabActive)}
              >
                Prompts
              </Link>
            ) : (
              <NavLink to={orgHref('/prompts')} className={({ isActive }) => pill(isActive)}>
                Prompts
              </NavLink>
            )}
            {/* Marketplace — within-org prompt/blueprint browse (TFAC-537),
                multi-mode only (no local route exists). No org-level toggle
                gates this: the within-org marketplace is always on for
                every multi-mode org (org_settings.marketplace_enabled is
                reserved for the future cross-org gate, TFAC-92 phase 2 /
                TFAC-539, and is unrelated to this surface). */}
            {isMulti && (
              <NavLink to={orgHref('/marketplace')} className={({ isActive }) => pill(isActive)}>
                Marketplace
              </NavLink>
            )}
            {orgAdmin && (
              <NavLink to={orgHref('/org')} className={({ isActive }) => pill(isActive)}>
                Org
              </NavLink>
            )}
            {fleetVisible && (
              <NavLink to={orgHref('/fleet')} className={({ isActive }) => pill(isActive)}>
                Fleet
              </NavLink>
            )}
          </div>
          <NavLink
            to={orgHref('/settings')}
            className={({ isActive }) =>
              `p-2 rounded-full transition-all duration-200 ${
                isActive
                  ? 'bg-warm-2 text-warm'
                  : 'text-ink-3 hover:text-ink-2 hover:bg-tint-2'
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
