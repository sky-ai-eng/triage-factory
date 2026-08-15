import { useEffect, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import {
  getGitHubAppStatus,
  startGitHubAppRegistration,
  type GitHubAppStatus,
} from '../../lib/githubApp'
import {
  Section,
  Field,
  inputClass,
  glassInputClass,
  segmentedWrap,
  segmentedBtn,
} from './primitives'

/**
 * GitHubAppPanel is the org/workspace-scope "GitHub access" block — the
 * App-registration alternative to a PAT. A registered App gives the workspace
 * a bot identity of its own, which outlives the person who set it up (a PAT
 * leaves with them), and moves per-team repository scoping into GitHub's
 * hands: a team's work runs on an installation token minted for that team's
 * repositories, and the token cannot reach past them — the limit is GitHub's,
 * not something TF enforces in code. It's self-contained (owns its
 * load/error/register state) so it drops identically into the Settings
 * workspace tab and the org-create configure step.
 *
 * Installation lives elsewhere now — the wizard's "Install the App" step and
 * Settings' "App installation" section own that affordance (one place per
 * surface), so the panel is registration plus the connected/registered display
 * only. It still refetches on window focus, which keeps the connected display's
 * installation count current after an install happens in another tab.
 *
 * orgId is null in multi mode until the active org resolves — stay loading
 * rather than fetch the wrong org. A focus-refetch failure keeps any
 * already-loaded data instead of flipping the panel to an error.
 */
export default function GitHubAppPanel({
  orgId,
  showHeading = true,
  bare = false,
  ownerType: ownerTypeProp,
  initialStatus,
  returnTo,
}: {
  orgId: string | null
  // Suppressed in the setup wizard, which already labels the step "GitHub
  // access" and shows an App/PAT tab switcher above this panel. Default true
  // keeps the Settings tab labelled.
  showHeading?: boolean
  // The setup wizard composes this flush (no Section card, glass fields) so it
  // matches the rest of the flow; Settings keeps the carded default.
  bare?: boolean
  // When provided, the owner type (personal vs org) is controlled from outside
  // and the internal Account-type toggle is hidden — the setup wizard picks it
  // in its own prior step. Absent (Settings) ⇒ the toggle is shown and the
  // choice is internal.
  ownerType?: 'user' | 'org'
  // A status the caller already has, so the panel renders its final content on
  // the first paint instead of a loading line. The setup wizard fetches this in
  // the step's load(), alongside every other step's, long before the step is
  // opened — which matters because the step animates open, and a body that
  // changes height mid-animation is a body that gets clipped. Absent
  // (Settings) ⇒ the panel loads it itself as before. The focus-refetch still
  // runs either way, so this is a head start, not a cache.
  initialStatus?: GitHubAppStatus
  // Where the post-registration callback should land the browser: 'setup' (the
  // wizard resumes on the install step) or 'settings' (back to this panel).
  // Required so every render site is explicit — a wizard reuse that forgot it
  // would silently drop the user on Settings mid-setup.
  returnTo: 'setup' | 'settings'
}) {
  const fieldCls = bare ? glassInputClass : inputClass
  // Discriminated so the panel can tell "still resolving" / "load failed"
  // apart from "no app registered" — a load failure must NOT render the
  // registration form.
  const [ghAppState, setGhAppState] = useState<
    { kind: 'loading' } | { kind: 'error' } | { kind: 'loaded'; status: GitHubAppStatus }
  >(initialStatus ? { kind: 'loaded', status: initialStatus } : { kind: 'loading' })
  const [ghReloadKey, setGhReloadKey] = useState(0)
  // Owner type is controlled when ownerTypeProp is set (the wizard's account-type
  // step), else internal (Settings shows the toggle).
  const ownerControlled = ownerTypeProp !== undefined
  const [ownerTypeInternal, setOwnerTypeInternal] = useState<'user' | 'org'>('user')
  const ghAppOwnerType = ownerTypeProp ?? ownerTypeInternal
  const [ghAppOwnerLogin, setGhAppOwnerLogin] = useState('')
  const [ghAppRegistering, setGhAppRegistering] = useState(false)
  const [ghAppDetailsExpanded, setGhAppDetailsExpanded] = useState(false)

  useEffect(() => {
    if (!orgId) return
    let cancelled = false
    const load = () => {
      getGitHubAppStatus(orgId)
        .then((s) => {
          if (!cancelled) setGhAppState({ kind: 'loaded', status: s })
        })
        .catch(() => {
          if (!cancelled) {
            setGhAppState((prev) => (prev.kind === 'loaded' ? prev : { kind: 'error' }))
          }
        })
    }
    load()
    const onFocus = () => load()
    window.addEventListener('focus', onFocus)
    return () => {
      cancelled = true
      window.removeEventListener('focus', onFocus)
    }
  }, [orgId, ghReloadKey])

  // registerGitHubApp leaves the page: it navigates to the backend launch
  // bounce page, which renders the manifest form under its own CSP and
  // POSTs to github.com on confirm. Control returns via the callback
  // redirect, not here.
  const registerGitHubApp = () => {
    if (!orgId) return
    setGhAppRegistering(true)
    startGitHubAppRegistration(orgId, {
      owner_type: ghAppOwnerType,
      owner_login: ghAppOwnerLogin.trim(),
      return_to: returnTo,
    })
  }

  const status = ghAppState.kind === 'loaded' ? ghAppState.status : null
  const app = status?.app ?? null

  const inner = (
    <>
      {showHeading && (
        <h2 className="text-[13px] font-medium text-text-secondary mb-1">GitHub access</h2>
      )}
      <p className="text-[11px] text-text-tertiary mb-4 leading-relaxed">
        A GitHub App connects Triage Factory to your organization under a bot identity of its own —
        one that doesn't leave with the person who set it up — and lets GitHub itself hold
        each team to its own repositories: a team's token is issued for the repositories that
        team tracks, and it can't reach past them. A Personal Access Token is the simpler
        alternative — you don't need both.
      </p>

      {ghAppState.kind === 'loading' && (
        <p className="text-[12px] text-text-tertiary italic">Loading GitHub App status…</p>
      )}

      {ghAppState.kind === 'error' && (
        <div className="flex items-center justify-between gap-2 rounded-xl bg-dismiss/[0.06] border border-dismiss/15 px-4 py-2.5">
          <span className="text-[12px] text-dismiss">Couldn&rsquo;t load GitHub App status.</span>
          <button
            type="button"
            onClick={() => setGhReloadKey((k) => k + 1)}
            className="shrink-0 text-[11px] text-accent hover:text-accent/80"
          >
            Retry
          </button>
        </div>
      )}

      {status &&
        (app ? (
          <div className="space-y-3">
            <button
              type="button"
              onClick={() => setGhAppDetailsExpanded((v) => !v)}
              aria-expanded={ghAppDetailsExpanded}
              className="w-full flex items-center gap-2 rounded-xl bg-claim/[0.06] border border-claim/15 px-4 py-2.5 text-left transition-colors hover:bg-claim/[0.1]"
            >
              <div className="w-1.5 h-1.5 rounded-full bg-claim shrink-0" />
              <span className="text-[12px] text-claim flex-1">
                Connected to GitHub via your own App ({app.slug})
              </span>
              {ghAppDetailsExpanded ? (
                <ChevronDown size={14} className="text-claim/70 shrink-0" />
              ) : (
                <ChevronRight size={14} className="text-claim/70 shrink-0" />
              )}
            </button>

            {ghAppDetailsExpanded && (
              <div className="text-[11px] text-text-tertiary space-y-0.5 px-1">
                <p>
                  App slug: <span className="text-text-secondary">{app.slug}</span>
                </p>
                {app.registered_at && (
                  <p>
                    Registered:{' '}
                    <span className="text-text-secondary">
                      {new Date(app.registered_at).toLocaleDateString()}
                    </span>
                    {app.registered_by_display_name ? ` by ${app.registered_by_display_name}` : ''}
                  </p>
                )}
                <p>
                  Installations:{' '}
                  <span className="text-text-secondary">{status.installations.length}</span>
                </p>
                {status.installations.length > 0 && (
                  <div className="space-y-1 pt-2">
                    {status.installations.map((inst) => (
                      <div
                        key={inst.installation_id}
                        className="flex items-center justify-between rounded-xl border border-border-subtle bg-white/40 px-3 py-2"
                      >
                        <span className="text-[12px] text-text-primary">{inst.account_login}</span>
                        <span className="text-[10px] uppercase tracking-wide text-text-tertiary">
                          {inst.account_type}
                        </span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
          </div>
        ) : (
          <div className="space-y-3">
            {!ownerControlled && (
              <Field label="Account type">
                <div className={segmentedWrap(bare)}>
                  {(['user', 'org'] as const).map((t) => (
                    <button
                      key={t}
                      type="button"
                      onClick={() => setOwnerTypeInternal(t)}
                      className={segmentedBtn(ghAppOwnerType === t, bare)}
                    >
                      {t === 'user' ? 'Personal' : 'Organization'}
                    </button>
                  ))}
                </div>
              </Field>
            )}
            <Field label={ghAppOwnerType === 'org' ? 'GitHub organization' : 'GitHub username'}>
              <input
                type="text"
                placeholder={ghAppOwnerType === 'org' ? 'your-org' : 'your-username'}
                value={ghAppOwnerLogin}
                onChange={(e) => setGhAppOwnerLogin(e.target.value)}
                className={fieldCls}
              />
            </Field>
            <button
              type="button"
              onClick={registerGitHubApp}
              disabled={ghAppRegistering || !ghAppOwnerLogin.trim()}
              className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
            >
              {ghAppRegistering ? 'Redirecting to GitHub…' : 'Register your own GitHub App'}
            </button>
            <p className="text-[11px] text-text-tertiary leading-relaxed">
              You&rsquo;ll be taken to GitHub to confirm the App, then returned here. GitHub will
              ask which repositories to grant on install.
            </p>
          </div>
        ))}
    </>
  )

  return bare ? inner : <Section>{inner}</Section>
}
