// The GitHub step body (mandatory step 1). GitHub is the backbone, so it gets a
// first-class two-stage flow — never grouped with the trackers:
//
//   1. URL reachability sub-step — prefill https://github.com (a one-tap
//      confirm for the common case; free entry for *.ghe.com data residency and
//      GHES). Verifies the host answers, then collapses to "GitHub URL: <host>".
//   2. Access sub-step — two tabs, GitHub App (default, both modes) vs PAT.
//      App registration rides GitHubAppPanel; the PAT path validates creds +
//      base URL together on Connect, surfacing the server's validation error
//      (no longer the swallowed-creds ambiguity).
//   3. Clone protocol — PAT path only, local mode only (multi forces HTTPS), so
//      it rides inside the PAT tab's GitHubAccessGroup rather than as a separate
//      card. The App path omits it entirely (App ⇒ HTTPS).
//
// Reuse rule: composes the shared GitHubAccessGroup (PAT-only mode) and
// GitHubAppPanel — no parallel field UIs. The base URL is owned here (the
// reachability sub-step) and suppressed inside the group via showBaseUrl.

import { useEffect, useRef, useState } from 'react'
import GitHubAccessGroup from '../settings/GitHubAccessGroup'
import GitHubAppPanel from '../settings/GitHubAppPanel'
import { saveOrgConfig } from '../settings/orgConfig'
import { checkGitHubReachability } from '../../lib/reachability'
import ReachabilitySubStep from './ReachabilitySubStep'
import type { StepContext } from './types'

const DEFAULT_GITHUB_URL = 'https://github.com'

export default function GitHubStep({ orgId, isLocal, state, patch }: StepContext) {
  // The URL is "confirmed" once it has passed a reachability check. A
  // GitHub-ready org (PAT or App from a prior run) skips straight to the access
  // view — its URL was confirmed before; an unconnected org always starts on
  // the reachability sub-step, even when a base URL is prefilled, so github.com
  // is the one-tap confirm the step is designed around.
  const [urlConfirmed, setUrlConfirmed] = useState(() => state.githubReady)
  // App is the default access method in both modes; a returning org that
  // already has a stored PAT resumes on the PAT tab so it reads as "this is how
  // you're connected" rather than nudging toward re-registering as an App.
  const [tab, setTab] = useState<'app' | 'pat'>(state.hasGitHubPat ? 'pat' : 'app')
  const [connecting, setConnecting] = useState(false)
  const [connectError, setConnectError] = useState<string | null>(null)

  // On the App path the base URL must be persisted before registration (the
  // launch endpoint derives the GitHub host from stored org settings). Persist
  // it once per distinct URL when the App tab is active — creds aren't touched
  // (the PAT field is blank), so this is the bare base-URL save, at the access
  // stage, not the reachability stage.
  const persistedUrlRef = useRef<string | null>(null)
  useEffect(() => {
    if (!urlConfirmed || tab !== 'app') return
    const url = state.org.github_url
    if (persistedUrlRef.current === url) return
    persistedUrlRef.current = url
    void saveOrgConfig(state.org).catch(() => {
      // Best-effort: a failure here just means registration may target the
      // wrong host; the PAT path's Connect re-persists URL+creds together.
      persistedUrlRef.current = null
    })
  }, [urlConfirmed, tab, state.org])

  const onUrlConfirmed = (url: string) => {
    patch({ org: { ...state.org, github_url: url } })
    setUrlConfirmed(true)
  }

  const connectPat = async () => {
    setConnecting(true)
    setConnectError(null)
    try {
      // saveOrgConfig POSTs the full org form; the backend validates the PAT
      // against the base URL and returns a 422 with a clear message on a bad
      // token — exactly the creds error the old candidate path swallowed.
      const result = await saveOrgConfig(state.org)
      if (!result.ok) {
        setConnectError(result.error)
        return
      }
      patch({ githubReady: true, hasGitHubPat: true, org: { ...state.org, github_pat: '' } })
    } finally {
      setConnecting(false)
    }
  }

  return (
    <div className="space-y-4">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        GitHub is the backbone — Triage Factory polls your repos for the PRs and reviews it
        surfaces. Confirm where your GitHub lives, then connect.
      </p>

      <ReachabilitySubStep
        label="GitHub URL"
        prefill={DEFAULT_GITHUB_URL}
        value={state.org.github_url || DEFAULT_GITHUB_URL}
        confirmed={urlConfirmed}
        check={checkGitHubReachability}
        onConfirm={onUrlConfirmed}
        onEdit={() => setUrlConfirmed(false)}
        helpText="github.com for the common case; a *.ghe.com data-residency subdomain or your GitHub Enterprise Server host otherwise."
      />

      {urlConfirmed && (
        <div className="space-y-3">
          {state.githubReady && (
            <div className="flex items-center gap-2 rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-2.5">
              <div className="h-1.5 w-1.5 shrink-0 rounded-full bg-claim" />
              <span className="text-[12px] text-claim">GitHub connected.</span>
            </div>
          )}

          {/* App (default) vs PAT — two tabs, the access sub-step proper. */}
          <div className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5">
            {(['app', 'pat'] as const).map((t) => (
              <button
                key={t}
                type="button"
                onClick={() => {
                  setTab(t)
                  setConnectError(null)
                }}
                className={`rounded-md px-3 py-1 text-[12px] font-medium transition-colors ${
                  tab === t
                    ? 'bg-white text-text-primary shadow-sm'
                    : 'text-text-tertiary hover:text-text-secondary'
                }`}
              >
                {t === 'app' ? 'GitHub App' : 'Personal Access Token'}
              </button>
            ))}
          </div>

          {tab === 'app' ? (
            orgId ? (
              <GitHubAppPanel orgId={orgId} />
            ) : (
              <p className="text-[12px] italic text-text-tertiary">Resolving your workspace…</p>
            )
          ) : (
            <div className="space-y-3">
              <GitHubAccessGroup
                value={{
                  github_url: state.org.github_url,
                  github_pat: state.org.github_pat,
                  github_clone_protocol: state.org.github_clone_protocol,
                }}
                onChange={(p) => patch({ org: { ...state.org, ...p } })}
                hasToken={state.hasGitHubPat}
                isLocal={isLocal}
                orgId={orgId}
                showAppPanel={false}
                showBaseUrl={false}
              />
              {connectError && (
                <div className="rounded-xl border border-dismiss/20 bg-dismiss/[0.08] px-4 py-2.5 text-[13px] text-dismiss">
                  {connectError}
                </div>
              )}
              <button
                type="button"
                onClick={connectPat}
                disabled={connecting || (!state.hasGitHubPat && state.org.github_pat.trim() === '')}
                className="rounded-xl bg-accent px-4 py-2.5 text-[13px] font-medium text-white transition-colors hover:bg-accent/90 disabled:opacity-40"
              >
                {connecting ? 'Connecting…' : state.githubReady ? 'Reconnect' : 'Connect GitHub'}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
