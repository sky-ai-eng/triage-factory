// The wizard's User-section Jira step: capture the signed-in user's own Jira
// access token. This is the Jira sibling of UserIdentityStep, with one
// difference baked into the copy and the flow: GitHub captures *identity* and
// discards the token; Jira captures *access* and STORES the token, because the
// user acts as themselves on board claims.
//
// DC = paste-a-PAT (this step). Cloud OAuth (one-click Connect) is a later
// Cloud-tier ticket; until it lands there's no Connect button — the PAT paste
// is the only capture path. The PAT path is Continue-driven (steps.tsx persist:
// validate the token, store it, derive the account, then mark connected).

import { glassInputClass } from '../settings/primitives'
import type { StepContext } from './types'

const JiraMark = () => (
  <svg viewBox="0 0 32 32" width="15" height="15" aria-hidden="true" className="fill-current">
    <path d="M30 16L16 2 2 16l7 7 5-5 2 2-7 7 2 2 14-14zM16 12l-4 4-2-2 6-6 6 6-2 2-4-4z" />
  </svg>
)

export function JiraUserAccessStep({ state, patch, error }: StepContext) {
  const host = state.jiraUserHost || 'Jira'

  // Already bound (a stored credential from a prior session or this one) — just
  // confirm it; the wizard's Continue finishes.
  if (state.jiraUserConnected) {
    return (
      <div className="space-y-5">
        <div className="space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
            Your Jira access
          </h2>
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            So Triage Factory can act as you on Jira — claiming and updating tickets under your
            name, not a shared bot.
          </p>
        </div>
        <div className="flex items-center gap-2.5 rounded-xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/50 px-4 py-3">
          <span className="text-[var(--color-claim)]">
            <JiraMark />
          </span>
          <p className="text-[13px] text-text-secondary">
            Connected as{' '}
            <span className="font-medium text-text-primary">{state.jiraUserAccount}</span>
            <span className="text-text-tertiary"> on {host}</span>
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Connect your Jira
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Triage Factory acts as you on{' '}
          <span className="font-medium text-text-secondary">{host}</span> — so the tickets it claims
          and updates are attributed to you, not a shared bot. Paste your personal Jira token to
          link your account.
        </p>
      </div>

      <div className="space-y-2">
        <label className="block">
          <span className="mb-2 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
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
                ? ' !border-[var(--color-dismiss)] focus:!border-[var(--color-dismiss)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
                : ''
            }`}
          />
        </label>
        <p className="text-[11px] leading-relaxed text-text-tertiary">
          Unlike GitHub, this token is stored — Triage Factory needs it to act as you on Jira. It
          stays in your workspace&rsquo;s secret store and is never shared with other users.
        </p>
      </div>
    </div>
  )
}
