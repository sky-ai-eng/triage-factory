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
import Brief from './pages/Brief'
import Settings from './pages/Settings'
import Prompts from './pages/Prompts'
import OrgTemplate from './pages/OrgTemplate'
import Repos from './pages/Repos'
import Factory from './pages/Factory'
import Projects from './pages/Projects'
import ProjectDetail from './pages/ProjectDetail'
import Login from './pages/Login'
import Onboarding from './pages/Onboarding'
import OrgConfigure from './pages/OrgConfigure'
import TeamConfigure from './pages/TeamConfigure'
import ConnectGitHub from './pages/ConnectGitHub'
import Shell from './Shell'
import AuthGate, { RequireGitHubIdentity } from './AuthGate'
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
          provisions the sentinel tenant and routes into the configure flow. */}
      <Route path="/start" element={<StartFactory />} />
      {/* Create-time configure steps — the SAME pages multi uses, wrapped in
          the local gate and flagged isLocal (SSH toggle + PAT, no App panel).
          Routed with the sentinel org id; team_id is the "default" alias the
          team endpoints resolve to the local Default team. Declared before the
          shell layout so the static /configure + /carry-over suffixes win. */}
      <Route
        path="/orgs/:org_id/configure"
        element={
          <AuthGate mode="local">
            <OrgConfigure isLocal />
          </AuthGate>
        }
      />
      <Route
        path="/orgs/:org_id/teams/:team_id/configure"
        element={
          <AuthGate mode="local">
            <TeamConfigure isLocal />
          </AuthGate>
        }
      />
      {/* Jira carry-over — the final local first-run step (migrated from the
          retired Setup wizard), reached from TeamConfigure on Finish when
          Jira is connected. */}
      <Route
        path="/orgs/:org_id/carry-over"
        element={
          <AuthGate mode="local">
            <JiraCarryOver />
          </AuthGate>
        }
      />
      <Route
        element={
          <AuthGate mode="local">
            <Shell />
          </AuthGate>
        }
      >
        <Route path="/" element={<Factory />} />
        <Route path="/triage" element={<Cards />} />
        <Route path="/board" element={<Board />} />
        <Route path="/board/runs/:runID" element={<RunDetail />} />
        <Route path="/prs" element={<PRDashboard />} />
        <Route path="/prompts" element={<Prompts />} />
        <Route path="/repos" element={<Repos />} />
        <Route path="/projects" element={<Projects />} />
        <Route path="/projects/:id" element={<ProjectDetail />} />
        <Route path="/brief" element={<Brief />} />
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
          <Route path="/no-orgs" element={<Navigate to="/onboarding" replace />} />
          {/* Redirect for stale /setup bookmarks — the legacy local setup
              wizard is retired; integration creds live in per-org Vault (D5)
              and are configured via the admin UI (D14). */}
          <Route path="/setup" element={<Navigate to="/" replace />} />
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
          {/* Create-time configure step (the second half of "Start your
              Factory"). Outside RequireGitHubIdentity for the same reason
              as connect-github — it's where GitHub access gets set up, so
              gating it on GitHub identity would loop. */}
          <Route
            path="/orgs/:org_id/configure"
            element={
              <AuthGate mode="multi">
                <OrgConfigure />
              </AuthGate>
            }
          />
          {/* Create-time team configure step — the second half of "Create
              your first team" and the tail of onboarding (org-configure
              routes here on Finish). Outside RequireGitHubIdentity for the
              same reason as configure: it's where repos + GitHub-team
              mappings get set up. Declared before the shell layout so the
              static /teams/:team_id/configure suffix wins. */}
          <Route
            path="/orgs/:org_id/teams/:team_id/configure"
            element={
              <AuthGate mode="multi">
                <TeamConfigure />
              </AuthGate>
            }
          />
          <Route
            path="/orgs/:org_id"
            element={
              <AuthGate mode="multi">
                <RequireGitHubIdentity>
                  <Shell />
                </RequireGitHubIdentity>
              </AuthGate>
            }
          >
            <Route index element={<Factory />} />
            <Route path="triage" element={<Cards />} />
            <Route path="board" element={<Board />} />
            <Route path="board/runs/:runID" element={<RunDetail />} />
            <Route path="prs" element={<PRDashboard />} />
            <Route path="prompts" element={<Prompts />} />
            {/* Org-template editor (SKY-381) — multi-mode only; the page
                itself redirects non-admins. No local-mode route (N=1 has no
                template); LocalRoutes' catch-all sends /org-template → /. */}
            <Route path="org-template" element={<OrgTemplate />} />
            <Route path="repos" element={<Repos />} />
            <Route path="projects" element={<Projects />} />
            <Route path="projects/:id" element={<ProjectDetail />} />
            <Route path="brief" element={<Brief />} />
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
