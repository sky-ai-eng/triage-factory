/* eslint-disable react-refresh/only-export-components --
   This is the SPA entrypoint. Inline route components (Loading,
   RootRedirect, *Routes) aren't exported anywhere — the file calls
   createRoot().render() at the bottom and exits. The rule's HMR
   heuristic doesn't apply to an entrypoint. */
import { StrictMode, Suspense, lazy } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Routes, Route, Navigate, useNavigate, useLocation } from 'react-router'
import { useEffect } from 'react'
import './index.css'
import { watchSystemTheme } from './lib/theme'

watchSystemTheme()
import StartFactory from './pages/StartFactory'
import JiraCarryOver from './pages/JiraCarryOver'
import Cards from './pages/Cards'
import Board from './pages/Board'
import RunDetail from './pages/RunDetail'
import PRDashboard from './pages/PRDashboard'
import Settings from './pages/Settings'
import Prompts from './pages/Prompts'
import OrgPage from './pages/OrgPage'
import TeamSettings from './pages/team/TeamSettings'
import Marketplace from './pages/Marketplace'
import Repos from './pages/Repos'
import Factory from './pages/Factory'
import Knowledge from './pages/knowledge/Knowledge'
import Fleet from './pages/Fleet'
import Usage from './pages/Usage'
import Overview from './pages/Overview'
import Login from './pages/Login'
import Onboarding from './pages/Onboarding'
import InviteAccept from './pages/InviteAccept'
import Wizard from './pages/setup/Wizard'
import ConnectGitHub from './pages/ConnectGitHub'
import ConnectJira from './pages/ConnectJira'
import Shell from './Shell'
import AuthGate, {
  RequireGitHubIdentity,
  RequireJiraIdentity,
  RequireSetupComplete,
} from './AuthGate'
import ToastProvider from './components/Toast/ToastProvider'
import { useDeploymentConfig } from './hooks/useDeploymentConfig'
import { AuthProvider, useAuth } from './contexts/AuthContext'
import { OrgProvider, useActiveOrgId } from './contexts/OrgContext'

/**
 * Top-level router branches on deployment_mode (read once from
 * /api/config). Local mode keeps the existing flat route table.
 * Multi mode mounts an org-prefixed shell route plus /login,
 * /onboarding, and a RootRedirect for bare / and unknown paths.
 *
 * AuthProvider + OrgProvider only wrap the multi-mode tree —
 * AuthContext is undefined in local mode, which is what
 * useOptionalAuth keys off to hide the OrgPicker + UserMenu.
 */

function Loading() {
  return (
    <div className="min-h-screen bg-ground flex items-center justify-center">
      <p className="text-ink-3 text-sm">Loading...</p>
    </div>
  )
}

/** RootRedirect resolves bare / and unknown paths in multi mode to
 *  /orgs/<active>. Sits inside AuthGate so unauth/no-orgs cases are
 *  handled before it tries to redirect. */
function RootRedirect() {
  const auth = useAuth()
  const activeOrgId = useActiveOrgId()
  const navigate = useNavigate()

  useEffect(() => {
    if (auth.status !== 'authed' || !activeOrgId) return
    navigate('/orgs/' + activeOrgId, { replace: true })
  }, [auth.status, activeOrgId, navigate])

  return <Loading />
}

function LocalRoutes() {
  return (
    <Routes>
      {/* First-run provision trigger. Unguarded — it's the !configured target
          LocalAuthGate redirects to (a gate here would loop). On Start it
          provisions the sentinel tenant and routes into the /setup wizard. */}
      <Route path="/start" element={<StartFactory />} />
      {/* Jira carry-over — the final local first-run step (migrated from the
          retired Setup wizard), reached from the /setup wizard's Finish when
          Jira is the connected tracker. Declared before the shell layout so
          the static /carry-over suffix wins. */}
      <Route
        path="/orgs/:org_id/carry-over"
        element={
          <AuthGate mode="local">
            <JiraCarryOver />
          </AuthGate>
        }
      />
      {/* The setup wizard — the single create-time flow the mandatory-setup
          gate redirects incomplete users to. Outside RequireSetupComplete
          (gating it here would loop the very user it's meant to onboard); the
          local gate still requires a provisioned tenant. */}
      <Route
        path="/setup"
        element={
          <AuthGate mode="local">
            <Wizard isLocal />
          </AuthGate>
        }
      />
      {/* Identity gate page — its own route OUTSIDE RequireGitHubIdentity (the
          check it satisfies) so there's no loop. The backstop for a local user
          whose identity is missing/stale outside the wizard; the wizard's User
          step is the first-run capture. Declared before the shell layout so the
          static /connect-github suffix wins. */}
      <Route
        path="/orgs/:org_id/connect-github"
        element={
          <AuthGate mode="local">
            <ConnectGitHub />
          </AuthGate>
        }
      />
      {/* Jira identity gate page — its own route OUTSIDE RequireJiraIdentity
          (the check it satisfies) so there's no loop. The backstop for a local
          user whose Jira credential is missing/stale; the wizard's User step is
          the first-run capture. Declared before the shell layout so the static
          /connect-jira suffix wins. */}
      <Route
        path="/orgs/:org_id/connect-jira"
        element={
          <AuthGate mode="local">
            <ConnectJira />
          </AuthGate>
        }
      />
      <Route
        element={
          <AuthGate mode="local">
            <RequireSetupComplete isLocal>
              <RequireGitHubIdentity isLocal>
                <RequireJiraIdentity isLocal>
                  <Shell />
                </RequireJiraIdentity>
              </RequireGitHubIdentity>
            </RequireSetupComplete>
          </AuthGate>
        }
      >
        <Route path="/" element={<Factory />} />
        {/* Overview is a stub page behind a real route: the rail's first row
            and the scope switcher both point here, and a nav row that 404s is
            worse than one that says "not yet". */}
        <Route path="/overview" element={<Overview />} />
        <Route path="/triage" element={<Cards />} />
        <Route path="/board" element={<Board />} />
        <Route path="/runs/:conversationID" element={<RunDetail />} />
        <Route path="/prs" element={<PRDashboard />} />
        {/* Team knowledge — the page in BOTH mode tables. Local has knowledge
            bases too: one team's worth, stored as plain files under the state
            root instead of in an object store. */}
        <Route path="/knowledge" element={<Knowledge />} />
        <Route path="/prompts" element={<Prompts />} />
        <Route path="/repos" element={<Repos />} />
        <Route path="/usage" element={<Usage />} />
        {/* Fleet console (TFAC-589) — mounted in both modes; the page itself
            gates on operator + FeatureFleet and bounces home when absent. */}
        <Route path="/fleet" element={<Fleet />} />
        <Route path="/settings" element={<Settings />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}

function MultiRoutes() {
  return (
    <AuthProvider>
      <OrgProvider>
        <Routes>
          <Route path="/login" element={<Login />} />
          {/* Unified zero-membership onboarding entry (create-or-invite).
              /no-orgs is kept as a redirect for any stale links. */}
          <Route path="/onboarding" element={<Onboarding />} />
          {/* Public invite-redemption page (TFAC-418). OUTSIDE every AuthGate
              — a brand-new invitee has no org/membership — but inside
              AuthProvider so it can tell signed-in from signed-out. It reads
              ?token=, previews unauthenticated, and accepts once signed in. */}
          <Route path="/invite/accept" element={<InviteAccept />} />
          <Route path="/no-orgs" element={<Navigate to="/onboarding" replace />} />
          {/* The setup wizard — the single create-time flow the mandatory-setup
              gate redirects incomplete users to. Outside RequireSetupComplete /
              RequireGitHubIdentity (gating it would loop the user it onboards);
              wrapped only in the authed+org-resolved gate. The wizard reads the
              active org from OrgContext, so the path stays a bare /setup. */}
          <Route
            path="/setup"
            element={
              <AuthGate mode="multi">
                <Wizard />
              </AuthGate>
            }
          />
          {/* Onboarding gate page — its own route so it sits OUTSIDE
              RequireGitHubIdentity (the check it exists to satisfy);
              gated only by auth + org resolution. Declared before the
              shell layout so the static /connect-github suffix wins. */}
          <Route
            path="/orgs/:org_id/connect-github"
            element={
              <AuthGate mode="multi">
                <ConnectGitHub />
              </AuthGate>
            }
          />
          {/* Jira onboarding gate page — its own route so it sits OUTSIDE
              RequireJiraIdentity (the check it exists to satisfy); gated only by
              auth + org resolution. Declared before the shell layout so the
              static /connect-jira suffix wins. */}
          <Route
            path="/orgs/:org_id/connect-jira"
            element={
              <AuthGate mode="multi">
                <ConnectJira />
              </AuthGate>
            }
          />
          <Route
            path="/orgs/:org_id"
            element={
              <AuthGate mode="multi">
                <RequireSetupComplete>
                  <RequireGitHubIdentity>
                    <RequireJiraIdentity>
                      <Shell />
                    </RequireJiraIdentity>
                  </RequireGitHubIdentity>
                </RequireSetupComplete>
              </AuthGate>
            }
          >
            <Route index element={<Factory />} />
            <Route path="overview" element={<Overview />} />
            <Route path="triage" element={<Cards />} />
            <Route path="board" element={<Board />} />
            <Route path="runs/:conversationID" element={<RunDetail />} />
            <Route path="prs" element={<PRDashboard />} />
            <Route path="knowledge" element={<Knowledge />} />
            {/* Prompts is its own destination. It used to redirect into a tab
                on /team, which is where the editor lived before the rail gave
                Prompts a row of its own with Library, Marketplace and Bindings
                under it. The page is mode-agnostic — it resolves its team from
                useActiveTeam like every other surface. */}
            <Route path="prompts" element={<Prompts />} />
            <Route path="repos" element={<Repos />} />
            <Route path="usage" element={<Usage />} />
            <Route path="fleet" element={<Fleet />} />
            {/* Org surface (TFAC-417) — multi-mode only; mounted under the
                /orgs/:org_id parent. Non-admins reaching it directly get a
                read-only roster (OrgPage gates management on org role). */}
            <Route path="org" element={<OrgPage />} />
            {/* Team page shell (TFAC-445) — [Members · Settings · Prompts]
                tabs + a shared team-switcher, plus the zero-team safe landing.
                Multi-mode only. */}
            <Route path="team" element={<TeamSettings />} />
            {/* Within-org prompt marketplace browse page (TFAC-537). Multi-mode
                only — nothing in LocalRoutes; absence from that table is the
                mode gate, mirroring org/team above. */}
            <Route path="marketplace" element={<Marketplace />} />
            <Route path="settings" element={<Settings />} />
          </Route>
          <Route
            path="/"
            element={
              <AuthGate mode="multi">
                <RootRedirect />
              </AuthGate>
            }
          />
          <Route
            path="*"
            element={
              <AuthGate mode="multi">
                <RootRedirect />
              </AuthGate>
            }
          />
        </Routes>
      </OrgProvider>
    </AuthProvider>
  )
}

/* The design-system gallery. `import.meta.env.DEV` is statically replaced at
   build time, so in production this is `false ? lazy(...) : null` and neither
   the component nor its stylesheet reaches the bundle. */
const UiGallery = import.meta.env.DEV ? lazy(() => import('./dev/UiGallery')) : null

function AppRoutes() {
  const { pathname } = useLocation()
  const { config, loading, error } = useDeploymentConfig()

  /* Intercepted ahead of the deployment-mode branch, and ahead of every gate:
     the gallery mounts ui components with hand-written props and talks to
     nothing, so it must stay reachable when the backend is down or the user is
     signed out. That independence is the point — it is where the design system
     is reviewed, including when the API is what you are busy breaking. */
  if (UiGallery && pathname.startsWith('/dev/ui')) {
    return (
      <Suspense fallback={<Loading />}>
        <UiGallery />
      </Suspense>
    )
  }

  if (loading) return <Loading />
  if (error || !config) {
    return (
      <div className="min-h-screen bg-ground flex items-center justify-center">
        <div className="text-center space-y-3">
          <p className="text-ink-2 text-sm">{error ?? 'Failed to load configuration'}</p>
          <button
            type="button"
            onClick={() => window.location.reload()}
            className="text-warm text-sm underline"
          >
            Retry
          </button>
        </div>
      </div>
    )
  }
  return config.deployment_mode === 'multi' ? <MultiRoutes /> : <LocalRoutes />
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <AppRoutes />
      <ToastProvider />
    </BrowserRouter>
  </StrictMode>,
)
