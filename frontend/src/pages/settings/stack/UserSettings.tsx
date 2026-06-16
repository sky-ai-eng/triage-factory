// The User group of the Settings stack — always rendered, for every role. The
// first durable home for GitHub-identity management (previously only the setup
// wizard's User step and the Connect gate page): show the bound @login + host,
// with a re-Connect / re-enter affordance, reusing the captured-and-discarded
// PAT_2 path. Plus device-local preferences (theme), which commit immediately.

import { useState } from 'react'
import { useGitHubIdentity } from '../../../hooks/useGitHubIdentity'
import { useJiraIdentity } from '../../../hooks/useJiraIdentity'
import { captureGitHubIdentityPat } from '../../../lib/githubIdentity'
import { captureJiraIdentityPat, captureJiraIdentityApiToken } from '../../../lib/jiraIdentity'
import { getStoredTheme, setTheme, type ThemeMode } from '../../../lib/theme'
import SettingsSection from './SettingsSection'

export default function UserSettings({ orgId }: { orgId: string | null }) {
  return (
    <div className="space-y-2.5">
      <GitHubIdentitySection orgId={orgId} />
      <JiraIdentitySection orgId={orgId} />
      <AppearanceSection />
    </div>
  )
}

// GitHubIdentitySection — Connect (one-click OAuth when an App is registered)
// or the PAT_2 paste; either way the end state is a verified identity row with
// no token retained. An action section (binds inline), so no Save footer.
function GitHubIdentitySection({ orgId }: { orgId: string | null }) {
  const { state, refresh } = useGitHubIdentity(orgId)
  const [pat, setPat] = useState('')
  const [capturing, setCapturing] = useState(false)
  const [patError, setPatError] = useState<string | null>(null)
  const [reentering, setReentering] = useState(false)

  const summary =
    state.status === 'ready'
      ? state.data.connected
        ? `@${state.data.login} · ${state.data.host}`
        : 'Not connected'
      : state.status === 'error'
        ? 'Status unavailable'
        : 'Loading…'

  const startConnect = () => {
    if (!orgId) return
    window.location.href =
      '/api/orgs/' +
      encodeURIComponent(orgId) +
      '/github/connect/start?return_to=' +
      encodeURIComponent('/settings')
  }

  const submitPat = async () => {
    if (!orgId || pat.trim() === '' || capturing) return
    setCapturing(true)
    setPatError(null)
    try {
      await captureGitHubIdentityPat(orgId, pat.trim())
      setPat('')
      setReentering(false)
      refresh()
    } catch (e) {
      setPatError(e instanceof Error ? e.message : 'Could not verify that token.')
    } finally {
      setCapturing(false)
    }
  }

  const connected = state.status === 'ready' && state.data.connected
  const connectAvailable = state.status === 'ready' && state.data.connect_available
  const host = state.status === 'ready' ? state.data.host : 'GitHub'

  return (
    <SettingsSection title="GitHub identity" summary={summary}>
      <div className="space-y-3">
        <p className="text-[12px] leading-relaxed text-text-tertiary">
          Triage Factory matches your pull requests and reviews to you by your GitHub username on{' '}
          <span className="text-text-secondary">{host}</span>. It only reads your username — it
          never stores your token or gains access to your repositories.
        </p>

        {connected && !reentering ? (
          <div className="flex items-center justify-between gap-3 rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-2.5">
            <span className="text-[12px] text-claim">
              Connected as @{state.status === 'ready' ? state.data.login : ''}
            </span>
            <button
              type="button"
              onClick={() => setReentering(true)}
              className="text-[11px] text-text-tertiary underline transition-colors hover:text-text-secondary"
            >
              {connectAvailable ? 'Reconnect' : 'Re-identify with a PAT'}
            </button>
          </div>
        ) : (
          <div className="space-y-2.5">
            {/* After an App→PAT teardown the org has no App client_id, so
                one-click Connect is gone (connect_available=false). Existing
                host-verified logins survive; anyone who needs to (re)bind uses
                the capture-and-discard PAT path — say so, so the missing Connect
                button doesn't read as a regression. */}
            {!connectAvailable && (
              <p className="text-[12px] leading-relaxed text-text-tertiary">
                This workspace switched to PAT access; identity capture now uses a personal token
                that is verified and discarded.
              </p>
            )}
            {connectAvailable && (
              <button
                type="button"
                onClick={startConnect}
                className="w-full rounded-xl bg-text-primary px-4 py-2.5 text-[13px] font-medium text-white transition-colors hover:bg-text-primary/90"
              >
                Connect GitHub
              </button>
            )}
            <label className="block">
              <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                {connectAvailable
                  ? 'Or paste a personal access token'
                  : 'Paste a personal access token'}
              </span>
              <input
                type="password"
                autoComplete="off"
                value={pat}
                placeholder="ghp_…"
                onChange={(e) => {
                  setPat(e.target.value)
                  if (patError) setPatError(null)
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void submitPat()
                }}
                aria-invalid={!!patError || undefined}
                className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary transition-colors focus:outline-none focus:ring-2 focus:ring-accent/30 ${
                  patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
                }`}
              />
            </label>
            {patError && <p className="text-[12px] leading-relaxed text-dismiss">{patError}</p>}
            <div className="flex items-center gap-2">
              <button
                type="button"
                onClick={() => void submitPat()}
                disabled={pat.trim() === '' || capturing}
                className="rounded-xl border border-border-glass px-4 py-2 text-[13px] font-medium text-text-secondary transition-colors hover:border-accent/40 hover:text-text-primary disabled:opacity-40"
              >
                {capturing ? 'Verifying…' : 'Verify token'}
              </button>
              {connected && reentering && (
                <button
                  type="button"
                  onClick={() => {
                    setReentering(false)
                    setPat('')
                    setPatError(null)
                  }}
                  className="text-[12px] text-text-tertiary transition-colors hover:text-text-secondary"
                >
                  Cancel
                </button>
              )}
            </div>
          </div>
        )}
      </div>
    </SettingsSection>
  )
}

// JiraIdentitySection — the Jira sibling of GitHubIdentitySection. DC =
// paste-a-PAT (no Connect button; Cloud OAuth is a later ticket). The
// structural difference from GitHub: the token is STORED (Jira's user level
// holds access, not just identity), so the copy is honest about retention. An
// action section (binds inline), so no Save footer.
function JiraIdentitySection({ orgId }: { orgId: string | null }) {
  const { state, refresh } = useJiraIdentity(orgId)
  const [pat, setPat] = useState('')
  const [email, setEmail] = useState('')
  const [apiToken, setApiToken] = useState('')
  const [capturing, setCapturing] = useState(false)
  const [patError, setPatError] = useState<string | null>(null)
  const [reentering, setReentering] = useState(false)

  // The credential shape follows the org's deployment: Cloud binds an Atlassian
  // email + API token, Data Center a single personal access token.
  const cloud = state.status === 'ready' && state.data.deployment === 'cloud'

  const summary =
    state.status === 'ready'
      ? state.data.connected
        ? `${state.data.account ?? ''} · ${state.data.host}`
        : 'Not connected'
      : state.status === 'error'
        ? 'Status unavailable'
        : 'Loading…'

  // submit captures whichever credential the deployment dictates, validates +
  // stores it, then refreshes the bound status.
  const submit = async () => {
    if (!orgId || capturing) return
    const ready = cloud ? email.trim() !== '' && apiToken.trim() !== '' : pat.trim() !== ''
    if (!ready) return
    setCapturing(true)
    setPatError(null)
    try {
      if (cloud) {
        await captureJiraIdentityApiToken(orgId, email.trim(), apiToken.trim())
      } else {
        await captureJiraIdentityPat(orgId, pat.trim())
      }
      setPat('')
      setEmail('')
      setApiToken('')
      setReentering(false)
      refresh()
    } catch (e) {
      setPatError(e instanceof Error ? e.message : 'Could not verify that credential.')
    } finally {
      setCapturing(false)
    }
  }

  const connected = state.status === 'ready' && state.data.connected
  const host = state.status === 'ready' ? state.data.host : 'Jira'
  const submitDisabled =
    capturing || (cloud ? email.trim() === '' || apiToken.trim() === '' : pat.trim() === '')

  // Jira is an optional tracker, so — unlike the always-shown GitHub section —
  // only surface this when the org actually has a Jira host configured. A
  // GitHub-only org reports host="" from the status endpoint; rendering anyway
  // would print the dangling "acts as you on " copy with an empty host. While
  // loading or on a read error we also render nothing (we can't yet tell), so
  // the broken copy never flashes. The wizard gates the sibling step the same
  // way (visible: jiraActive).
  if (!(state.status === 'ready' && state.data.host !== '')) return null

  return (
    <SettingsSection title="Jira identity" summary={summary}>
      <div className="space-y-3">
        <p className="text-[12px] leading-relaxed text-text-tertiary">
          Triage Factory acts as you on <span className="text-text-secondary">{host}</span> — so the
          tickets it claims and updates are attributed to you, not a shared bot. Unlike GitHub, your
          token is stored (it&rsquo;s needed to act as you); it stays in your workspace&rsquo;s
          secret store and is never shared with other users.
        </p>

        {connected && !reentering ? (
          <div className="flex items-center justify-between gap-3 rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-2.5">
            <span className="text-[12px] text-claim">
              Connected as {state.status === 'ready' ? state.data.account : ''}
            </span>
            <button
              type="button"
              onClick={() => setReentering(true)}
              className="text-[11px] text-text-tertiary underline transition-colors hover:text-text-secondary"
            >
              Reconnect
            </button>
          </div>
        ) : (
          <div className="space-y-2.5">
            {cloud ? (
              <>
                <label className="block">
                  <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                    Atlassian account email
                  </span>
                  <input
                    type="email"
                    autoComplete="email"
                    value={email}
                    placeholder="you@yourcompany.com"
                    onChange={(e) => {
                      setEmail(e.target.value)
                      if (patError) setPatError(null)
                    }}
                    aria-invalid={!!patError || undefined}
                    className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary transition-colors focus:outline-none focus:ring-2 focus:ring-accent/30 ${
                      patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
                    }`}
                  />
                </label>
                <label className="block">
                  <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                    API token
                  </span>
                  <input
                    type="password"
                    autoComplete="off"
                    value={apiToken}
                    placeholder="Your Atlassian API token"
                    onChange={(e) => {
                      setApiToken(e.target.value)
                      if (patError) setPatError(null)
                    }}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') void submit()
                    }}
                    aria-invalid={!!patError || undefined}
                    className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary transition-colors focus:outline-none focus:ring-2 focus:ring-accent/30 ${
                      patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
                    }`}
                  />
                </label>
                <p className="text-[11px] leading-relaxed text-text-tertiary">
                  Create a token at{' '}
                  <a
                    href="https://id.atlassian.com/manage-profile/security/api-tokens"
                    target="_blank"
                    rel="noreferrer"
                    className="text-accent hover:underline"
                  >
                    Atlassian API token settings
                  </a>
                  .
                </p>
              </>
            ) : (
              <label className="block">
                <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                  Paste a personal access token
                </span>
                <input
                  type="password"
                  autoComplete="off"
                  value={pat}
                  placeholder="Your Jira token"
                  onChange={(e) => {
                    setPat(e.target.value)
                    if (patError) setPatError(null)
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') void submit()
                  }}
                  aria-invalid={!!patError || undefined}
                  className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary transition-colors focus:outline-none focus:ring-2 focus:ring-accent/30 ${
                    patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
                  }`}
                />
              </label>
            )}
            {patError && <p className="text-[12px] leading-relaxed text-dismiss">{patError}</p>}
            <div className="flex items-center gap-2">
              <button
                type="button"
                onClick={() => void submit()}
                disabled={submitDisabled}
                className="rounded-xl border border-border-glass px-4 py-2 text-[13px] font-medium text-text-secondary transition-colors hover:border-accent/40 hover:text-text-primary disabled:opacity-40"
              >
                {capturing ? 'Verifying…' : cloud ? 'Verify credential' : 'Verify token'}
              </button>
              {connected && reentering && (
                <button
                  type="button"
                  onClick={() => {
                    setReentering(false)
                    setPat('')
                    setEmail('')
                    setApiToken('')
                    setPatError(null)
                  }}
                  className="text-[12px] text-text-tertiary transition-colors hover:text-text-secondary"
                >
                  Cancel
                </button>
              )}
            </div>
          </div>
        )}
      </div>
    </SettingsSection>
  )
}

// AppearanceSection — theme. Persists to localStorage immediately, so no Save.
function AppearanceSection() {
  const [theme, setThemeState] = useState<ThemeMode>(() => getStoredTheme())
  return (
    <SettingsSection title="Appearance" summary={theme[0].toUpperCase() + theme.slice(1)}>
      <div>
        <span className="mb-2 block text-[11px] text-text-tertiary">Theme</span>
        <div className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5">
          {(['light', 'dark', 'auto'] as const).map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => {
                setThemeState(m)
                setTheme(m)
              }}
              className={`rounded-md px-3 py-1 text-[12px] font-medium capitalize transition-colors ${
                theme === m
                  ? 'bg-white text-text-primary shadow-sm'
                  : 'text-text-tertiary hover:text-text-secondary'
              }`}
            >
              {m}
            </button>
          ))}
        </div>
        <p className="mt-1.5 text-[11px] text-text-tertiary">
          Auto follows your system preference.
        </p>
      </div>
    </SettingsSection>
  )
}
