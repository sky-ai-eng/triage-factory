// The wizard's User-section Linear step: bind the signed-in user's own Linear
// access — the JiraUserAccessStep sibling. Triage Factory acts as each person
// in Linear for the writes they make, so their credential is stored, not just
// their username.
//
// One-click Connect is offered when a Linear OAuth app resolves for the org
// (linearUserConnectAvailable): an external redirect that returns to /setup,
// where the step's load re-reads the status and resumes complete. The key
// paste is always offered and is the step's Continue (steps.tsx persist:
// validate the key, store it, derive the account, then mark connected).

import { glassInputClass } from '../settings/primitives'
import { linearConnectStartURL } from '../../lib/linearIdentity'
import type { StepContext } from './types'

export function LinearUserAccessStep({ state, patch, orgId, error }: StepContext) {
  const workspace = state.linearWorkspaceUrlKey
    ? `linear.app/${state.linearWorkspaceUrlKey}`
    : 'Linear'
  const connectAvailable = state.linearUserConnectAvailable

  const startConnect = () => {
    if (!orgId) return
    window.location.href = linearConnectStartURL(orgId, '/setup')
  }

  if (state.linearUserConnected) {
    return (
      <div className="space-y-5">
        <div className="space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Your Linear access</h2>
          <p className="text-body leading-relaxed text-ink-3">
            So Triage Factory can act as you in Linear — claiming and updating issues under your
            name, not a shared bot.
          </p>
        </div>
        <div className="flex items-center gap-2.5 rounded-xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/50 px-4 py-3">
          <p className="text-body text-ink-2">
            Connected as <span className="font-medium text-ink-1">{state.linearUserAccount}</span>
            <span className="text-ink-3"> in {workspace}</span>
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Connect your Linear</h2>
        <p className="text-body leading-relaxed text-ink-3">
          Triage Factory acts as you in <span className="font-medium text-ink-2">{workspace}</span>{' '}
          — so the issues it claims and updates are attributed to you, not a shared bot.
          {connectAvailable
            ? ' Authorize with Linear, or paste a personal API key, to link your account.'
            : ' Paste a personal Linear API key to link your account.'}
        </p>
      </div>

      {connectAvailable && (
        <button
          type="button"
          onClick={startConnect}
          className="flex w-full items-center justify-center gap-2 rounded-xl bg-inverse px-4 py-2.5 text-body font-medium text-inverse-ink transition-colors hover:bg-inverse/90"
        >
          Connect Linear
        </button>
      )}

      <div className="space-y-2">
        <label className="block">
          <span className="mb-2 block text-reported font-medium uppercase tracking-wide text-ink-3">
            {connectAvailable ? 'Or paste a personal API key' : 'Personal API key'}
          </span>
          <input
            type="password"
            autoComplete="off"
            value={state.linearUserApiKey}
            placeholder="lin_api_…"
            onChange={(e) => patch({ linearUserApiKey: e.target.value })}
            aria-invalid={!!error || undefined}
            className={`${glassInputClass}${
              error
                ? ' !border-[var(--color-alarm)] focus:!border-[var(--color-alarm)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
                : ''
            }`}
          />
        </label>
        <p className="text-reported leading-relaxed text-ink-3">
          Create a key in Linear under Settings → Account → Security &amp; Access. Whichever way you
          connect, your key or token is stored in your organization&rsquo;s secret store so Triage
          Factory can act as you, and is never shared with other users.
        </p>
      </div>
    </div>
  )
}
