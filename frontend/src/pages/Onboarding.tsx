import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router'
import { useAuth } from '../contexts/AuthContext'
import { apiJSON, HttpError } from '../lib/apiClient'

/**
 * Onboarding is the unified entry for an authenticated user with zero
 * org memberships — the steady-state first-run landing in multi mode
 * (signup provisions nothing; org creation is a deliberate action).
 *
 * One page, two affordances, gated by a single instance flag
 * (`me.org_creation_enabled`, the inverse of TF_PREVENT_ORG_CREATION):
 *
 *   - creation allowed (default) → "Create your organization": a single
 *     org-name field + the "Start your Factory" CTA, plus a passive "or
 *     wait for an invite" note. Submitting POSTs /api/orgs (create with
 *     default settings), then refreshes /me — the membership-appears effect
 *     below routes into the /setup wizard.
 *   - creation prevented → the invite-only state: "ask your admin",
 *     no create affordance.
 *
 * Local mode never mounts AuthContext, so this is multi-mode only.
 */
export default function Onboarding() {
  const auth = useAuth()
  const navigate = useNavigate()

  const [orgName, setOrgName] = useState('')
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)
  // Set to the id of an org this session just created, so the
  // membership-appears effect routes the founder into the /setup wizard
  // rather than straight into the (unconfigured) app. An invite-driven
  // membership (createdOrgId still null) routes into the app as before —
  // that org is already configured by its own admin.
  const [createdOrgId, setCreatedOrgId] = useState<string | null>(null)

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    const name = orgName.trim()
    if (name === '' || creating) {
      return
    }
    setCreating(true)
    setCreateError(null)
    try {
      const created = await apiJSON<{ id: string }>('/api/orgs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      })
      setCreatedOrgId(created.id)
    } catch (err) {
      // Only the create call is guarded here: nothing was persisted, so
      // re-enable the form for a retry. (refresh() below folds its own
      // failures into auth status and never throws, so it can't be
      // misreported as a create failure.)
      setCreateError(
        err instanceof HttpError && err.status === 403
          ? 'Organization creation is disabled on this instance.'
          : 'Failed to create organization. Please try again.',
      )
      setCreating(false)
      return
    }
    // The org now exists with the caller as owner and its active org set
    // server-side. Re-fetch /me so the membership lands; the effect below
    // sees orgs.length > 0 and routes into the app. Keep `creating` true
    // through the redirect so the button stays disabled.
    await auth.refresh()
  }

  // Redirect to /login once logout flips auth to unauth. Onboarding is
  // outside AuthGate so nothing else observes this transition.
  useEffect(() => {
    if (auth.status === 'unauth') {
      navigate('/login', { replace: true })
    }
  }, [auth.status, navigate])

  // When a membership appears (e.g. an admin adds the user to an org, or
  // the create-org flow completes), route onward without a reload. A
  // just-created org goes to the /setup wizard (the single create-time flow,
  // org resolved from OrgContext's server-active pick); an invite-driven
  // membership goes straight into the app.
  //
  // The primary invite entry is the public /invite/accept page (TFAC-418),
  // which refreshes /me and routes into the joined org itself. This effect is
  // the coherent fallback: a user sitting on this "wait for an invite" screen
  // who accepts in another tab (or whose membership otherwise lands) and hits
  // Refresh flows through here — createdOrgId is null, so they go straight into
  // the org rather than the founder's /setup wizard.
  useEffect(() => {
    if (auth.status !== 'authed' || auth.orgs.length === 0) return
    if (createdOrgId && auth.orgs.some((o) => o.id === createdOrgId)) {
      navigate('/setup', { replace: true })
      return
    }
    navigate('/orgs/' + auth.orgs[0].id, { replace: true })
  }, [auth.status, auth.orgs, createdOrgId, navigate])

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

            <form onSubmit={handleCreate} className="space-y-2">
              <label htmlFor="org-name" className="sr-only">
                Organization name
              </label>
              <input
                id="org-name"
                type="text"
                value={orgName}
                onChange={(e) => setOrgName(e.target.value)}
                disabled={creating}
                maxLength={200}
                placeholder="Organization name"
                autoFocus
                className="w-full bg-white/50 border border-border-subtle focus:border-accent focus:outline-none text-text-primary placeholder:text-text-tertiary rounded-xl px-4 py-2.5 text-[13px] transition-colors disabled:opacity-50"
              />
              <button
                type="submit"
                disabled={creating || orgName.trim() === ''}
                className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 disabled:cursor-not-allowed text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
              >
                {creating ? 'Creating…' : 'Start your Factory'}
              </button>
              {createError && <p className="text-[11px] text-red-500 text-center">{createError}</p>}
            </form>

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
