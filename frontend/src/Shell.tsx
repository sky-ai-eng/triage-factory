import { useCallback, useMemo } from 'react'
import { Outlet, useLocation, useNavigate } from 'react-router'
import UiShell from './ui/shell/Shell'
import type { Grant, RailCounts } from './ui/shell/routes'
import { useOrgHref } from './hooks/useOrgHref'
import { useRailCounts } from './hooks/useRailCounts'
import { useWsConnected } from './hooks/useWebSocket'
import { useOptionalAuth } from './contexts/AuthContext'
import { useOrgRole } from './hooks/useOrgRole'
import { useActiveTeam, useTeams } from './hooks/useTeams'
import type { ShellScope } from './hooks/useShellScope'
import { FeatureFleet, useEntitlements } from './hooks/useEntitlements'
import { ChromeProvider } from './contexts/ChromeContext'

// The application frame. This file is the ADAPTER: it resolves who the viewer
// is, translates the design system's route ids into this app's paths, and hands
// the result to src/ui/shell, which knows about tokens and nothing else.
//
// The split is not ceremony. It is what lets /dev/ui mount the rail in all four
// grant permutations side by side, which is the only way the rail's behaviour
// across grants gets reviewed — and that behaviour IS the design. It also keeps
// the permission model in one array (ui/shell/routes.ts) that both the rail and
// the command palette read, so the palette can never offer a page the rail is
// hiding.

/**
 * Route id → client path. The design system names destinations; this map is the
 * only place those names meet React Router.
 *
 * Two ids resolve to the same page on purpose: Prompts' Library and Bindings
 * are one surface today, so both land on /prompts. The reverse map below drops
 * Bindings so Library is the canonical answer, which is what lights the rail.
 */
const PATHS: Record<string, string> = {
  overview: '/overview',
  board: '/board',
  factory: '/',
  knowledge: '/knowledge',
  repos: '/repos',
  pulls: '/prs',
  'prompts.library': '/prompts',
  'prompts.market': '/marketplace',
  'prompts.bindings': '/prompts',
  usage: '/usage',
  team: '/team',
  org: '/org',
  fleet: '/fleet',
  queue: '/triage',
  settings: '/settings',
}

/** Longest path first, so a nested path resolves to its own id and not to /. */
const BY_PATH: Array<[string, string]> = Object.entries(PATHS)
  .filter(([id]) => id !== 'prompts.bindings')
  .map(([id, path]) => [path, id] as [string, string])
  .sort((a, b) => b[0].length - a[0].length)

/** Which rail row is lit, given where the browser actually is. */
function routeIdFor(pathname: string): string {
  // Multi mode prefixes every path with /orgs/<id>; strip it so one map serves
  // both deployment shapes rather than two near-identical ones.
  const rel = pathname.replace(/^\/orgs\/[^/]+/, '') || '/'
  for (const [path, id] of BY_PATH) {
    if (path === '/') continue
    if (rel === path || rel.startsWith(path + '/')) return id
  }
  return rel === '/' ? 'factory' : ''
}

const TITLES: Record<string, string> = {
  overview: 'Overview',
  board: 'Board',
  factory: 'Factory',
  knowledge: 'Knowledge',
  repos: 'Repositories',
  pulls: 'Pull requests',
  'prompts.library': 'Prompts',
  'prompts.market': 'Marketplace',
  usage: 'Usage',
  team: 'Team',
  org: 'Organization',
  fleet: 'Fleet',
  queue: 'Queue',
  settings: 'Settings',
}

export default function Shell() {
  const orgHref = useOrgHref()
  const navigate = useNavigate()
  const { pathname } = useLocation()

  // useOptionalAuth returns the multi-mode auth context if present, null in
  // local mode. That absence IS the mode signal — there is no separate flag.
  const auth = useOptionalAuth()
  const isMulti = auth !== null

  const { isAdmin: orgAdmin } = useOrgRole()
  const { teams } = useTeams()
  const { has, loaded: entLoaded } = useEntitlements()

  const grants = useMemo<Grant[]>(() => {
    if (!isMulti) return []
    const g: Grant[] = []
    if (orgAdmin) g.push('org-admin')
    // Team admin is per-team and comes from the viewer's own membership rows —
    // an org admin is deliberately NOT a team admin here, which is why this
    // reads the roles rather than falling back on orgAdmin. The rows come from
    // /api/me rather than the teams-list cache because the AuthGate already
    // blocks first paint on /api/me: read from there, the whole grants picture
    // resolves in one paint instead of the rail rendering a shape it then
    // changes when a second request lands.
    if ((auth?.me?.teams ?? []).some((t) => t.role === 'admin')) g.push('team-admin')
    // The fleet console composes two deployment-scoped gates: the operator
    // identity from /api/me and the entitlement. Both must hold, matching the
    // backend's 404-and-hide.
    if (entLoaded && has(FeatureFleet) && (auth?.me?.is_operator ?? false)) g.push('fleet')
    return g
  }, [isMulti, orgAdmin, entLoaded, has, auth])

  const onRoute = useCallback(
    (id: string) => {
      const path = PATHS[id]
      if (path) navigate(orgHref(path))
    },
    [navigate, orgHref],
  )

  const activeOrg = auth?.orgs?.find((o) => o.id === auth?.serverActiveOrgId) ?? auth?.orgs?.[0]
  const teamRows = useMemo(() => teams.map((t) => ({ id: t.id, name: t.name })), [teams])

  // The org · team mark is the SCOPE CONTROL for pages that carry none of
  // their own (the Overview, by design): one persisted selection, seeded like
  // every single-team page scope, resolved to a concrete team the moment the
  // teams list loads (the sole team when there is only one). The resolved team
  // rides the outlet context so switching in the rail reframes the page in the
  // same render that moves the mark.
  const { teamId: pickedTeamId, setTeamId: setPickedTeamId } = useActiveTeam('shell')
  const scopeTeam = teams.find((t) => t.id === pickedTeamId) ?? teams[0] ?? null
  const scope = useMemo<ShellScope>(
    () => ({ teamId: scopeTeam?.id ?? '', teamName: scopeTeam?.name ?? '' }),
    [scopeTeam?.id, scopeTeam?.name],
  )
  // The shell reports the picked team's ID — names are display-only there,
  // since nothing makes them unique.
  const onTeamChange = setPickedTeamId

  // Rail tails are facts about destinations, and every one of them needs a
  // `total` this API does not return yet (backend-needs.md, items 1-8). Absent
  // rather than zero: a tail reading 0 says "no repositories", which is a claim,
  // and the rail has no business making it before it knows.
  const counts: RailCounts = {}

  // The three live counts, and whether the stream feeding them is up. Each
  // count reads `--` until its own first answer; `offline` inerts all three at
  // once, because a count nobody is maintaining is worse than no count.
  //
  // Read on every route, the full-bleed one below included. Not an oversight:
  // the rail's numbers stay current while a run is open, so coming back from
  // one shows the counts rather than three dashes filling in — and what it
  // costs to keep them is three COUNTs per burst of hints.
  const { needs, running, queued } = useRailCounts()
  const connected = useWsConnected()

  const route = routeIdFor(pathname)

  // Full-bleed routes (the agent run station) drop the frame so the page owns
  // the whole viewport — it is a focused, open-in-new-tab surface.
  if (/\/runs\/[^/]+\/?$/.test(pathname)) {
    return <Outlet />
  }

  return (
    // The provider wraps the shell rather than sitting inside it, so a page can
    // ask for the whole screen and have the answer reach the chrome above it.
    <ChromeProvider>
      {({ immersive, header }) => (
        <UiShell
          immersive={immersive}
          offline={!connected}
          mode={isMulti ? 'multi' : 'local'}
          grants={grants}
          flags={{
            marketplace: isMulti,
            // Governance has a rail row and a warm alert tail in the design, and no
            // page. Off until one exists — absent, not disabled, applied to us.
            governance: false,
          }}
          org={activeOrg?.name ?? (isMulti ? '' : 'Triage Factory')}
          team={scope.teamId ? { id: scope.teamId, name: scope.teamName } : null}
          teams={teamRows}
          onTeamChange={onTeamChange}
          route={route}
          onRoute={onRoute}
          // The header band is the shell's; what fills it is the page's. A page
          // that has something better to say sends it over ChromeContext; until
          // one does, the rail's name for where you are is the honest answer.
          crumbs={header?.crumbs}
          title={header?.title ?? TITLES[route] ?? ''}
          subtitle={header?.subtitle}
          actions={header?.actions}
          onTitleSave={header?.onTitleSave}
          needs={needs}
          running={running}
          queued={queued}
          counts={counts}
          user={
            isMulti && auth?.me
              ? { name: auth.me.display_name || auth.me.email || 'You', email: auth.me.email || '' }
              : null
          }
          onSignOut={isMulti ? () => void auth?.logout() : undefined}
        >
          <Outlet context={scope} />
        </UiShell>
      )}
    </ChromeProvider>
  )
}
