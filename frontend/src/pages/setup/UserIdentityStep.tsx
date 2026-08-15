// The wizard's User-section body: capture the signed-in user's own GitHub
// identity (their @login on the org's host). This is PAT_2 — distinct from, and
// independent of, the org's GitHub credential (PAT_1). It satisfies the same
// check the RequireGitHubIdentity gate enforces, in-flow, so a user who
// finishes the wizard never hits the standalone Connect gate page.
//
// Two capture paths, both ending in the identical state (a verified identity
// row, no token retained):
//   - Connect (one-click OAuth) when the org has a registered GitHub App.
//   - PAT_2 paste otherwise (and as the universal fallback): the wizard's
//     Continue validates the token, reads the login, and discards it.
//
// The Connect button is an external redirect (an OAuth round-trip can't fold
// into Continue); it returns to /setup, where the step's load re-reads identity
// and resumes complete. The PAT path is Continue-driven (steps.tsx persist).

import { glassInputClass } from '../settings/primitives'
import type { StepContext } from './types'

const GitHubMark = () => (
  <svg viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" className="fill-current">
    <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
  </svg>
)

export function UserIdentityStep({ state, patch, orgId, error }: StepContext) {
  const host = state.userIdentityHost || 'GitHub'

  // Already bound (a github.com login_claim, a prior Connect, or this session's
  // capture) — just confirm it; the wizard's Continue finishes.
  if (state.userIdentityConnected) {
    return (
      <div className="space-y-5">
        <div className="space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
            Your GitHub identity
          </h2>
          <p className="text-body leading-relaxed text-ink-3">
            So Triage Factory can match the pull requests and reviews you author to you.
          </p>
        </div>
        <div className="flex items-center gap-2.5 rounded-xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/50 px-4 py-3">
          <span className="text-[var(--color-claim)]">
            <GitHubMark />
          </span>
          <p className="text-body text-ink-2">
            Connected as <span className="font-medium text-ink-1">@{state.userIdentityLogin}</span>
            <span className="text-ink-3"> on {host}</span>
          </p>
        </div>
      </div>
    )
  }

  const startConnect = () => {
    if (!orgId) return
    window.location.href =
      '/api/orgs/' + encodeURIComponent(orgId) + '/github/connect/start?return_to=/setup'
  }

  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Connect your GitHub</h2>
        <p className="text-body leading-relaxed text-ink-3">
          Triage Factory needs to know who you are on{' '}
          <span className="font-medium text-ink-2">{host}</span> so it can match your pull requests
          and reviews to you. This reads only your username — it doesn&rsquo;t grant access to your
          repositories.
        </p>
      </div>

      {state.userConnectAvailable && (
        <button
          type="button"
          onClick={startConnect}
          className="flex w-full items-center justify-center gap-2 rounded-xl bg-inverse px-4 py-2.5 text-body font-medium text-inverse-ink transition-colors hover:bg-inverse/90"
        >
          <GitHubMark />
          Connect GitHub
        </button>
      )}

      <div className="space-y-2">
        <label className="block">
          <span className="mb-2 block text-reported font-medium uppercase tracking-wide text-ink-3">
            {state.userConnectAvailable
              ? 'Or paste a personal access token'
              : 'Personal access token'}
          </span>
          <input
            type="password"
            autoComplete="off"
            value={state.userGitHubPat}
            placeholder="ghp_…"
            onChange={(e) => patch({ userGitHubPat: e.target.value })}
            aria-invalid={!!error || undefined}
            className={`${glassInputClass}${
              error
                ? ' !border-[var(--color-alarm)] focus:!border-[var(--color-alarm)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
                : ''
            }`}
          />
        </label>
        <p className="text-reported leading-relaxed text-ink-3">
          {state.userConnectAvailable
            ? 'A token works too — only your username is read, and the token isn’t stored.'
            : 'Paste a token with read access to your account. Only your username is read; the token isn’t stored.'}
        </p>
      </div>
    </div>
  )
}
