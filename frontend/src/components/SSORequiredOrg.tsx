import { useAuth } from '../contexts/AuthContext'
import { useSwitchOrg } from '../hooks/useSwitchOrg'
import type { AuthOrg } from '../types'

/**
 * SSORequiredOrg stands in for an org's pages when the org requires signing in
 * through its own single sign-on and this session was not: every org-scoped
 * call there answers 403 SSO_REQUIRED, so instead of a page of failed reads the
 * viewer gets the way in — that org's sign-in, a full-page navigation to its
 * start URL. The shell and its org picker are not rendered here, so the other
 * orgs this session can use are offered beside it.
 */
export default function SSORequiredOrg({ org }: { org: AuthOrg }) {
  const auth = useAuth()
  const switchOrg = useSwitchOrg()
  const others = auth.orgs.filter((o) => o.id !== org.id && !o.sso_required)

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
