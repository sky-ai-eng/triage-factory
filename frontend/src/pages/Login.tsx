import { useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router'
import { useAuth } from '../contexts/AuthContext'
import { apiJSON } from '../lib/apiClient'

/**
 * Identifier-first login (TFAC-427). In a multi-org build an anonymous
 * visitor's org is unknown, so we can't show an org-specific SSO button up
 * front. Instead the visitor types their work email; we look up the exact
 * domain via POST /api/sso/discover and either redirect into that org's SAML
 * connection (start_url) or fall back to GitHub.
 *
 * GitHub is ALWAYS present — it's the universal bootstrap-floor login. A fresh
 * deployment has no SSO configured yet, so the first admin signs in via GitHub
 * and sets SSO up for everyone else. Discovery never reveals org identity: it
 * answers only "SSO available + where to start" or "not".
 *
 * If the user is already authed (e.g. someone bookmarked /login but has a valid
 * session), redirect straight to /. AuthGate then routes them into their org.
 */

interface DiscoverResponse {
  sso: boolean
  start_url?: string
}

// Notice is the post-submit hint shown under the email field. 'no-sso' is a
// definitive negative (discovery answered, this domain has no SSO); 'error' is
// "couldn't check" (the request failed) — kept distinct so a transient
// network/5xx never tells a user with SSO that they have none.
type Notice = null | 'no-sso' | 'error'

export default function Login() {
  const auth = useAuth()
  const location = useLocation()
  const navigate = useNavigate()

  const [email, setEmail] = useState('')
  const [checking, setChecking] = useState(false)
  // notice surfaces a post-submit hint without hiding the always-present GitHub
  // button — either "no SSO for this domain" or "couldn't check, try again".
  const [notice, setNotice] = useState<Notice>(null)

  useEffect(() => {
    if (auth.status === 'authed') {
      navigate('/', { replace: true })
    }
  }, [auth.status, navigate])

  // Return-to: prefer the ?return_to= query param (set by AuthGate when
  // redirecting unauthenticated users away from a protected route), else the
  // current pathname if it's not /login.
  const params = new URLSearchParams(location.search)
  const explicit = params.get('return_to')
  const fallback = location.pathname !== '/login' ? location.pathname : '/'
  const returnTo = explicit ?? fallback

  // ?error=sso_required is set by the OAuth callback when a GitHub
  // login is rejected because the org requires SSO on that email's domain. The
  // fix is to sign in via SSO — exactly what entering the work email here does
  // (identifier-first routes the verified domain to SSO).
  const ssoRequired = params.get('error') === 'sso_required'

  // ?error=seat_limit is set by the OAuth callback when the deployment has hit
  // its licensed seat cap and this would be a new user beyond it. The fix is an
  // administrative one (buy more seats / free one up), so the message points
  // there rather than offering a self-serve retry.
  const seatLimit = params.get('error') === 'seat_limit'

  // ?error=login_failed / ?error=login_cancelled are set by the OAuth callback
  // when the provider (GitHub, or GoTrue relaying it) hands back an error instead
  // of an auth code — a transient upstream fault vs. the user declining consent.
  // Both are recoverable by retrying, so the banner is an informative retry hint,
  // not a dead end.
  const loginFailed = params.get('error') === 'login_failed'
  const loginCancelled = params.get('error') === 'login_cancelled'

  const startGitHub = () => {
    window.location.href = '/api/auth/oauth/github?return_to=' + encodeURIComponent(returnTo)
  }

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const trimmed = email.trim()
    if (!trimmed || checking) return

    setChecking(true)
    setNotice(null)
    try {
      const res = await apiJSON<DiscoverResponse>('/api/sso/discover', {
        method: 'POST',
        body: JSON.stringify({ email: trimmed, return_to: returnTo }),
      })
      if (res.sso && res.start_url) {
        // Hand off to the SP-initiated SAML start endpoint (the same one the
        // My Apps bookmark tile points at). The page navigates away.
        window.location.href = res.start_url
        return
      }
      // Discovery answered: no SSO for this domain — keep the visitor on GitHub.
      setNotice('no-sso')
    } catch {
      // The request itself failed (network / 5xx) — we DON'T know whether SSO
      // exists, so we must not claim "no SSO". Surface a retry hint; GitHub is
      // always available so the user is never trapped.
      setNotice('error')
    } finally {
      setChecking(false)
    }
  }

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-sm backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Triage Factory
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            Enter your work email to continue.
          </p>
        </div>

        {auth.status === 'error' && auth.error && (
          <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
            Couldn&apos;t reach the server. Try again in a moment.
          </div>
        )}

        {ssoRequired && (
          <div
            role="alert"
            className="rounded-xl bg-accent/[0.08] border border-accent/20 px-4 py-2.5 text-[13px] text-text-secondary"
          >
            Your organization requires single sign-on. Enter your work email to continue via SSO.
          </div>
        )}

        {seatLimit && (
          <div
            role="alert"
            className="rounded-xl bg-accent/[0.08] border border-accent/20 px-4 py-2.5 text-[13px] text-text-secondary"
          >
            This deployment has reached its licensed seat limit. Ask your administrator to add seats
            before signing in.
          </div>
        )}

        {loginFailed && (
          <div
            role="alert"
            className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss"
          >
            Sign-in couldn&apos;t be completed. This is usually a temporary problem with the
            identity provider — please try again in a moment.
          </div>
        )}

        {loginCancelled && (
          <div
            role="alert"
            className="rounded-xl bg-accent/[0.08] border border-accent/20 px-4 py-2.5 text-[13px] text-text-secondary"
          >
            Sign-in was cancelled. Try again below to continue.
          </div>
        )}

        <form onSubmit={onSubmit} className="space-y-3">
          <div className="space-y-1.5">
            <label
              htmlFor="login-email"
              className="block text-[12px] font-medium text-text-secondary"
            >
              Work email
            </label>
            <input
              id="login-email"
              type="email"
              autoComplete="email"
              autoFocus
              value={email}
              onChange={(e) => {
                setEmail(e.target.value)
                if (notice) setNotice(null)
              }}
              placeholder="you@company.com"
              className="w-full rounded-xl border border-border-subtle bg-white/60 px-3.5 py-2.5 text-[13px] text-text-primary placeholder:text-text-tertiary focus:border-accent focus:bg-white focus:outline-none"
            />
          </div>

          {notice === 'no-sso' && (
            <p role="status" className="text-[12px] text-text-tertiary leading-relaxed">
              No single sign-on for that email. Continue with GitHub below.
            </p>
          )}

          {notice === 'error' && (
            <p role="alert" className="text-[12px] text-dismiss leading-relaxed">
              Couldn&apos;t check single sign-on right now. Try again, or continue with GitHub
              below.
            </p>
          )}

          <button
            type="submit"
            disabled={checking || !email.trim()}
            className="w-full flex items-center justify-center gap-2 bg-accent hover:bg-accent/90 disabled:opacity-50 disabled:cursor-not-allowed text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {checking ? 'Checking…' : 'Continue'}
          </button>
        </form>

        <div className="flex items-center gap-3">
          <div className="h-px flex-1 bg-border-subtle" />
          <span className="text-[11px] uppercase tracking-wide text-text-tertiary">or</span>
          <div className="h-px flex-1 bg-border-subtle" />
        </div>

        <button
          type="button"
          onClick={startGitHub}
          className="w-full flex items-center justify-center gap-2 bg-surface-inverse hover:bg-surface-inverse/90 text-text-inverse font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          <svg
            viewBox="0 0 16 16"
            width="16"
            height="16"
            aria-hidden="true"
            className="fill-current"
          >
            <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
          </svg>
          Sign in with GitHub
        </button>

        <p className="text-[11px] text-text-tertiary leading-relaxed">
          By signing in, you authorize Triage Factory to read your GitHub identity. Repository
          access is configured separately by your organization admin.
        </p>
      </div>
    </div>
  )
}
