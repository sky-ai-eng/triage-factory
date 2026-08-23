// The wizard's User-section Jira step: capture the signed-in user's own Jira
// access token. This is the Jira sibling of UserIdentityStep, with one
// difference baked into the copy and the flow: GitHub captures *identity* and
// discards the token; Jira captures *access* and STORES the token, because the
// user acts as themselves on board claims.
//
// DC = paste-a-PAT (this step). Cloud adds a one-click Connect (OAuth 3LO) when
// an Atlassian app resolves (jiraUserConnectAvailable) — an external redirect
// that returns to /setup, where the step's load re-reads identity and resumes
// complete; the paste path stays as the universal fallback. The PAT path is
// Continue-driven (steps.tsx persist: validate the token, store it, derive the
// account, then mark connected).

import { glassInputClass } from '../settings/primitives'
import type { StepContext } from './types'

const JiraMark = () => (
  <svg viewBox="0 0 32 32" width="15" height="15" aria-hidden="true" className="fill-current">
    <path d="M30 16L16 2 2 16l7 7 5-5 2 2-7 7 2 2 14-14zM16 12l-4 4-2-2 6-6 6 6-2 2-4-4z" />
  </svg>
)

export function JiraUserAccessStep({ state, patch, orgId, error }: StepContext) {
  const host = state.jiraUserHost || 'Jira'
  // The credential fields follow the org's deployment: Cloud binds an Atlassian
  // email + API token (Basic), Data Center a single personal access token.
  const cloud = state.jiraDeployment === 'cloud'
  // One-click Connect is offered when an Atlassian OAuth app resolves for the
  // org (Cloud only). An OAuth round-trip can't fold into Continue, so it's an
  // external redirect back to /setup; the paste path stays available below.
  const connectAvailable = state.jiraUserConnectAvailable

  const startConnect = () => {
    if (!orgId) return
    window.location.href =
      '/api/orgs/' + encodeURIComponent(orgId) + '/jira/connect/start?return_to=/setup'
  }

  // Already bound (a stored credential from a prior session or this one) — just
  // confirm it; the wizard's Continue finishes.
  if (state.jiraUserConnected) {
    return (
      <div className="space-y-5">
        <div className="space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Your Jira access</h2>
          <p className="text-body leading-relaxed text-ink-3">
            So Triage Factory can act as you on Jira — claiming and updating tickets under your
            name, not a shared bot.
          </p>
        </div>
        <div className="flex items-center gap-2.5 rounded-xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/50 px-4 py-3">
          <span className="text-warm">
            <JiraMark />
          </span>
          <p className="text-body text-ink-2">
            Connected as <span className="font-medium text-ink-1">{state.jiraUserAccount}</span>
            <span className="text-ink-3"> on {host}</span>
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Connect your Jira</h2>
        <p className="text-body leading-relaxed text-ink-3">
          Triage Factory acts as you on <span className="font-medium text-ink-2">{host}</span> — so
          the tickets it claims and updates are attributed to you, not a shared bot.
          {connectAvailable
            ? ' Authorize with Atlassian, or paste a token, to link your account.'
            : ' Paste your personal Jira token to link your account.'}
        </p>
      </div>

      {connectAvailable && (
        <button
          type="button"
          onClick={startConnect}
          className="flex w-full items-center justify-center gap-2 rounded-xl bg-inverse px-4 py-2.5 text-body font-medium text-inverse-ink transition-colors hover:bg-inverse/90"
        >
          <JiraMark />
          Connect Jira
        </button>
      )}

      <div className="space-y-2">
        {connectAvailable && (
          <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
            {cloud ? 'Or paste an email + API token' : 'Or paste a personal access token'}
          </span>
        )}
        {cloud ? (
          <>
            <label className="block">
              <span className="mb-2 block text-reported font-medium uppercase tracking-wide text-ink-3">
                Atlassian account email
              </span>
              <input
                type="email"
                autoComplete="email"
                value={state.jiraUserEmail}
                placeholder="you@yourcompany.com"
                onChange={(e) => patch({ jiraUserEmail: e.target.value })}
                aria-invalid={!!error || undefined}
                className={`${glassInputClass}${
                  error
                    ? ' !border-[var(--color-alarm)] focus:!border-[var(--color-alarm)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
                    : ''
                }`}
              />
            </label>
            <label className="block">
              <span className="mb-2 block text-reported font-medium uppercase tracking-wide text-ink-3">
                API token
              </span>
              <input
                type="password"
                autoComplete="off"
                value={state.jiraUserApiToken}
                placeholder="Your Atlassian API token"
                onChange={(e) => patch({ jiraUserApiToken: e.target.value })}
                aria-invalid={!!error || undefined}
                className={`${glassInputClass}${
                  error
                    ? ' !border-[var(--color-alarm)] focus:!border-[var(--color-alarm)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
                    : ''
                }`}
              />
            </label>
            <p className="text-reported leading-relaxed text-ink-3">
              Create a token at{' '}
              <a
                href="https://id.atlassian.com/manage-profile/security/api-tokens"
                target="_blank"
                rel="noreferrer"
                className="text-warm hover:underline"
              >
                Atlassian API token settings
              </a>
              . Unlike GitHub, it&rsquo;s stored — Triage Factory needs it to act as you on Jira. It
              stays in your workspace&rsquo;s secret store and is never shared with other users.
            </p>
          </>
        ) : (
          <>
            <label className="block">
              <span className="mb-2 block text-reported font-medium uppercase tracking-wide text-ink-3">
                Personal access token
              </span>
              <input
                type="password"
                autoComplete="off"
                value={state.jiraUserPat}
                placeholder="Your Jira token"
                onChange={(e) => patch({ jiraUserPat: e.target.value })}
                aria-invalid={!!error || undefined}
                className={`${glassInputClass}${
                  error
                    ? ' !border-[var(--color-alarm)] focus:!border-[var(--color-alarm)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
                    : ''
                }`}
              />
            </label>
            <p className="text-reported leading-relaxed text-ink-3">
              Unlike GitHub, this token is stored — Triage Factory needs it to act as you on Jira.
              It stays in your workspace&rsquo;s secret store and is never shared with other users.
            </p>
          </>
        )}
      </div>
    </div>
  )
}
