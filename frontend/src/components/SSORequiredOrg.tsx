import { useEffect } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useSwitchOrg } from '../hooks/useSwitchOrg'
import type { AuthOrg } from '../types'

/** How often the interstitial re-reads /api/me while it is shown. */
const RECHECK_INTERVAL_MS = 30000

/**
 * SSORequiredOrg is shown in place of an org's pages when the org requires
 * signing in through its own single sign-on and this session was not: every
 * org-scoped call there answers 403 SSO_REQUIRED, so the user gets a link to
 * the org's sign-in (a full-page navigation to its start URL) instead of pages
 * whose reads all fail. The shell and its org picker are not rendered here, so
 * the other orgs this session can use are listed beside it.
 *
 * A barred session has no websocket to hear that the requirement was lifted,
 * so the interstitial re-reads /api/me on an interval and whenever the tab
 * becomes visible or focused; once the org no longer requires SSO, the gate
 * renders its pages again.
 */
export default function SSORequiredOrg({ org }: { org: AuthOrg }) {
  const auth = useAuth()
  const switchOrg = useSwitchOrg()
  const others = auth.orgs.filter((o) => o.id !== org.id && !o.sso_required)
  const { revalidate } = auth

  // Background re-reads: a failed one keeps this screen up, and the next
  // tick or focus tries again.
  useEffect(() => {
    const recheck = () => {
      if (document.visibilityState === 'visible') void revalidate()
    }
    const timer = window.setInterval(recheck, RECHECK_INTERVAL_MS)
    window.addEventListener('focus', recheck)
    document.addEventListener('visibilitychange', recheck)
    return () => {
      window.clearInterval(timer)
      window.removeEventListener('focus', recheck)
      document.removeEventListener('visibilitychange', recheck)
    }
  }, [revalidate])

  return (
    <div className="min-h-screen bg-ground flex items-center justify-center px-4">
      <div className="max-w-md text-center space-y-4">
        <p className="text-ui font-medium text-ink-3">{org.name}</p>
        <h1 className="text-section font-semibold text-ink-1 tracking-tight">
          This organization requires single sign-on
        </h1>
        {org.sso_start_url && (
          <a
            href={org.sso_start_url}
            className="inline-block rounded-full bg-warm px-4 py-2 text-body font-medium text-warm-ink transition-all hover:opacity-90"
          >
            Sign in with single sign-on
          </a>
        )}
        {others.length > 0 && (
          <div className="pt-4 space-y-2">
            <p className="text-ui text-ink-3">Or switch to</p>
            <div className="flex flex-wrap justify-center gap-2">
              {others.map((o) => (
                <button
                  key={o.id}
                  type="button"
                  onClick={() => void switchOrg(o.id)}
                  className="rounded-full border border-line-1 px-3.5 py-1.5 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1 hover:bg-tint-2"
                >
                  {o.name}
                </button>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
