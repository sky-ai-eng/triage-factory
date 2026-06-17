import { useEffect } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '../contexts/AuthContext'

/**
 * Login screen. One option: "Sign in with GitHub". The button kicks
 * the browser to /api/auth/oauth/github with the current return_to so
 * we land back on the page the user originally wanted.
 *
 * If the user is already authed (e.g. someone bookmarked /login but
 * has a valid session), redirect them straight to /. AuthGate will
 * then route them into their active org.
 */
export default function Login() {
  const auth = useAuth()
  const location = useLocation()
  const navigate = useNavigate()

  useEffect(() => {
    if (auth.status === 'authed') {
      navigate('/', { replace: true })
    }
  }, [auth.status, navigate])

  // Return-to: prefer the ?return_to= query param (set by AuthGate
  // when redirecting unauthenticated users away from a protected
  // route), else the current pathname if it's not /login.
  const params = new URLSearchParams(location.search)
  const explicit = params.get('return_to')
  const fallback = location.pathname !== '/login' ? location.pathname : '/'
  const returnTo = explicit ?? fallback

  const startGitHub = () => {
    const target = '/api/auth/oauth/github?return_to=' + encodeURIComponent(returnTo)
    window.location.href = target
  }

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-sm backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Triage Factory
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">Sign in to continue.</p>
        </div>

        {auth.status === 'error' && auth.error && (
          <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
            Couldn&apos;t reach the server. Try again in a moment.
          </div>
        )}

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
