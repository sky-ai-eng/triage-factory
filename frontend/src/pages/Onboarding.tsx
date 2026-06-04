import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../contexts/AuthContext'

/**
 * Onboarding is the unified entry for an authenticated user with zero
 * org memberships — the steady-state first-run landing in multi mode
 * (signup provisions nothing; org creation is a deliberate action).
 *
 * One page, two affordances, gated by a single instance flag
 * (`me.org_creation_enabled`, the inverse of TF_PREVENT_ORG_CREATION):
 *
 *   - creation allowed (default) → "Create your organization" with the
 *     "Start your Factory" CTA, plus a passive "or wait for an invite"
 *     note. The create-org flow itself is a follow-up ticket, so the CTA
 *     is present-but-disabled for now (clearly noted).
 *   - creation prevented → the invite-only state: "ask your admin",
 *     no create affordance.
 *
 * Local mode never mounts AuthContext, so this is multi-mode only.
 */
export default function Onboarding() {
  const auth = useAuth()
  const navigate = useNavigate()

  // Redirect to /login once logout flips auth to unauth. Onboarding is
  // outside AuthGate so nothing else observes this transition.
  useEffect(() => {
    if (auth.status === 'unauth') {
      navigate('/login', { replace: true })
    }
  }, [auth.status, navigate])

  // When a membership appears (e.g. an admin adds the user to an org, or
  // the create-org flow completes), route into the app without a reload.
  useEffect(() => {
    if (auth.status === 'authed' && auth.orgs.length > 0) {
      navigate('/orgs/' + auth.orgs[0].id, { replace: true })
    }
  }, [auth.status, auth.orgs, navigate])

  // This page lives outside MultiAuthGate and manages its own auth
  // observations, so it handles loading/error/unauth itself rather than
  // flashing the create-enabled UI before me (and org_creation_enabled)
  // are known.
  if (auth.status === 'loading') {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <p className="text-text-tertiary text-sm">Loading...</p>
      </div>
    )
  }
  if (auth.status === 'error') {
    // Unlike 'unauth' (which the effect above redirects to /login), an
    // error has no redirect — surface it with a retry instead of an
    // indefinite spinner.
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <div className="text-center space-y-3">
          <p className="text-text-secondary text-sm">{auth.error ?? 'Failed to load session'}</p>
          <button
            type="button"
            onClick={() => void auth.refresh()}
            className="text-accent text-sm underline"
          >
            Retry
          </button>
        </div>
      </div>
    )
  }
  // unauth: the redirect effect above fires within a frame.
  if (auth.status !== 'authed' || !auth.me) {
    return null
  }

  const accountLabel = auth.me.display_name || auth.me.email || 'this account'
  const canCreate = auth.me.org_creation_enabled

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-md backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        {canCreate ? (
          <>
            <div className="space-y-1.5">
              <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
                Welcome to Triage Factory
              </h1>
              <p className="text-[13px] text-text-tertiary leading-relaxed">
                You&apos;re signed in as{' '}
                <span className="text-text-secondary font-medium">{accountLabel}</span>. Create an
                organization to get started, or wait for an invite to an existing one.
              </p>
            </div>

            <div className="space-y-2">
              <button
                type="button"
                disabled
                title="Organization setup is coming soon"
                className="w-full bg-accent disabled:opacity-40 disabled:cursor-not-allowed text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
              >
                Start your Factory
              </button>
              <p className="text-[11px] text-text-tertiary text-center">
                Organization setup is coming soon.
              </p>
            </div>

            <div className="flex items-center gap-3">
              <div className="h-px flex-1 bg-border-subtle" />
              <span className="text-[11px] uppercase tracking-wide text-text-tertiary">or</span>
              <div className="h-px flex-1 bg-border-subtle" />
            </div>

            <p className="text-[13px] text-text-tertiary leading-relaxed">
              Waiting for an invite? Ask an administrator to add you to an organization, then
              refresh.
            </p>
          </>
        ) : (
          <div className="space-y-1.5">
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Invitation required
            </h1>
            <p className="text-[13px] text-text-tertiary leading-relaxed">
              You&apos;re signed in as{' '}
              <span className="text-text-secondary font-medium">{accountLabel}</span>. This instance
              is configured to require an invitation before granting access.
            </p>
            <p className="text-[13px] text-text-tertiary leading-relaxed">
              Contact your administrator to request an invite, then refresh or log back in with the
              invited account.
            </p>
          </div>
        )}

        <div className="flex gap-3">
          <button
            type="button"
            onClick={() => void auth.refresh()}
            className="flex-1 bg-white/50 hover:bg-white/80 border border-border-subtle text-text-secondary font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            Refresh
          </button>
          <button
            type="button"
            onClick={() => void auth.logout()}
            className="flex-1 bg-accent hover:bg-accent/90 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            Log out
          </button>
        </div>
      </div>
    </div>
  )
}
