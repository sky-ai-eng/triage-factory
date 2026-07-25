import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router'
import { useAuthStatus } from '../hooks/useAuthStatus'

/**
 * StartFactory is the local-mode first-run landing — the local analog of
 * multi's Onboarding "create your org" state. A brand-new install boots
 * tenant-less, so the app routes (behind LocalAuthGate) redirect an
 * unconfigured user here. There's no org-name field: local provisions the
 * fixed synthetic sentinel tenant, so the only input is the deliberate
 * "Start your factory" action that fires POST /api/setup/start
 * (idempotent — see handleSetupStart). On success we route into the /setup
 * wizard; the wizard route's own LocalAuthGate re-reads
 * /api/integrations/status and now sees configured=true.
 *
 * Local-mode only — multi mounts Onboarding instead (org creation is a
 * named-org POST, not a single sentinel provision).
 */
export default function StartFactory() {
  const navigate = useNavigate()
  const { configured, loading } = useAuthStatus()
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState('')

  // Already provisioned (a reload after Start, or a returning user who hit
  // /start directly) → straight into the app. The app routes are gated by
  // LocalAuthGate, which sends an unconfigured user back here, so this is the
  // single place that needs the inverse redirect.
  useEffect(() => {
    if (!loading && configured) navigate('/', { replace: true })
  }, [loading, configured, navigate])

  const start = async () => {
    setStarting(true)
    setError('')
    try {
      // No body: the endpoint provisions the fixed LocalDefault sentinel and
      // is idempotent, so a double-click or a reload-then-retry can't create
      // a second tenant or re-seed over user changes.
      const res = await fetch('/api/setup/start', { method: 'POST' })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        setError(data.error || 'Failed to start Triage Factory')
        return
      }
      // Provisioned. Route into the setup wizard; its LocalAuthGate re-reads
      // status and now admits us (configured=true).
      navigate('/setup', { replace: true })
    } catch {
      setError('Could not connect to server')
    } finally {
      setStarting(false)
    }
  }

  if (loading) {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <p className="text-text-tertiary text-sm">Loading...</p>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-md backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Welcome to Triage Factory
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            Triage Factory runs entirely on your machine — state lives in a local database and your
            tokens stay in your OS keychain. Get started by setting up your workspace, then connect
            GitHub and (optionally) Jira.
          </p>
        </div>

        <button
          type="button"
          onClick={start}
          disabled={starting}
          className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 disabled:cursor-not-allowed text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          {starting ? 'Starting…' : 'Start your factory'}
        </button>

        {error && <p className="text-[11px] text-red-500 text-center">{error}</p>}
      </div>
    </div>
  )
}
