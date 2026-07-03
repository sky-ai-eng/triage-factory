/* eslint-disable react-refresh/only-export-components --
   This is the SPA entrypoint. Inline route components (Loading,
   RootRedirect, *Routes) aren't exported anywhere — the file calls
   createRoot().render() at the bottom and exits. The rule's HMR
   heuristic doesn't apply to an entrypoint. */
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Routes, Route, Navigate, useNavigate } from 'react-router-dom'
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
import TeamPage from './pages/TeamPage'
import Marketplace from './pages/Marketplace'
import Repos from './pages/Repos'
import Factory from './pages/Factory'
import Projects from './pages/Projects'
import ProjectDetail from './pages/ProjectDetail'
import Usage from './pages/Usage'
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
    <div className="min-h-screen bg-surface flex items-center justify-center">
      <p className="text-text-tertiary text-sm">Loading...</p>
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
        <Route path="/triage" element={<Cards />} />
        <Route path="/board" element={<Board />} />
        <Route path="/runs/:runID" element={<RunDetail />} />
        <Route path="/prs" element={<PRDashboard />} />
        <Route path="/prompts" element={<Prompts />} />
        <Route path="/repos" element={<Repos />} />
        <Route path="/projects" element={<Projects />} />
        <Route path="/projects/:id" element={<ProjectDetail />} />
        <Route path="/usage" element={<Usage />} />
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
            <Route path="triage" element={<Cards />} />
            <Route path="board" element={<Board />} />
            <Route path="runs/:runID" element={<RunDetail />} />
            <Route path="prs" element={<PRDashboard />} />
            {/* Prompts is the /team Prompts tab in multi mode (TFAC-445). The
                top-level Prompts nav deep-links straight there; this redirect
                keeps the standalone /prompts path (and old bookmarks) pointing
                at the canonical surface. `..` pops to /orgs/:org_id. */}
            <Route path="prompts" element={<Navigate to="../team?tab=prompts" replace />} />
            {/* Legacy org-template route (SKY-381) — the standalone editor and
                its top-bar pill are gone (TFAC-436); the org template lives at
                the /org Template tab. Redirect old bookmarks/links to the
                canonical surface. Relative `..` pops to the /orgs/:org_id
                parent, preserving the active org. */}
            <Route path="org-template" element={<Navigate to="../org?tab=template" replace />} />
            <Route path="repos" element={<Repos />} />
            <Route path="projects" element={<Projects />} />
            <Route path="projects/:id" element={<ProjectDetail />} />
            <Route path="usage" element={<Usage />} />
            {/* Org surface (TFAC-417) — multi-mode only; mounted under the
                /orgs/:org_id parent. Non-admins reaching it directly get a
                read-only roster (OrgPage gates management on org role). */}
            <Route path="org" element={<OrgPage />} />
            {/* Team page shell (TFAC-445) — [Members · Settings · Prompts]
                tabs + a shared team-switcher, plus the zero-team safe landing.
                Multi-mode only. */}
            <Route path="team" element={<TeamPage />} />
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

function AppRoutes() {
  const { config, loading, error } = useDeploymentConfig()
  if (loading) return <Loading />
  if (error || !config) {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <div className="text-center space-y-3">
          <p className="text-text-secondary text-sm">{error ?? 'Failed to load configuration'}</p>
          <button
            type="button"
            onClick={() => window.location.reload()}
            className="text-accent text-sm underline"
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
