// The GitHub backbone, now two atomic stack entries instead of one two-stage
// step (GitHub is the backbone, so it's never grouped with the trackers):
//
//   GitHubUrlStep   — the base-URL field. Continue runs the URL-only
//                     reachability probe (no separate "Verify" button); an
//                     unreachable host bounces Continue and turns the box red.
//                     Prefilled https://github.com (a one-tap confirm for the
//                     common case; free entry for *.ghe.com data residency and
//                     GHES). The probe + base-URL save live in the step's
//                     persist (steps.tsx), so this body is just the field.
//   GitHubAccessStep — two tabs, GitHub App (default, both modes) vs PAT. The
//                     App tab keeps its external "Register your own GitHub App"
//                     launch (GitHubAppPanel) — a redirect can't fold into
//                     Continue. The PAT tab has NO separate Connect button:
//                     Continue performs the connect (validates creds + base URL,
//                     surfacing the server's error), per the step's persist.
//                     Clone protocol (PAT path, local only) rides inside the
//                     PAT tab's GitHubAccessGroup; the App path omits it.
//
// Reuse rule: composes the shared GitHubAccessGroup (PAT-only mode, base URL
// suppressed) and GitHubAppPanel — no parallel field UIs. The selected tab is
// lifted into wizard state (state.githubAccessTab) so the access step's
// persist can branch on it without reaching into this body.

import GitHubAccessGroup from '../settings/GitHubAccessGroup'
import GitHubAppPanel from '../settings/GitHubAppPanel'
import { UrlField } from './parts'
import type { StepContext } from './types'

export const DEFAULT_GITHUB_URL = 'https://github.com'

// GitHubUrlStep body — just the base-URL field. The reachability probe and the
// base-URL persist are the step's Continue (steps.tsx); `error` turns the box
// red when that probe failed, while the message renders in the host's shared
// error line.
export function GitHubUrlStep({ state, patch, error }: StepContext) {
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Where does your GitHub live?
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          GitHub is the backbone — Triage Factory polls your organization&rsquo;s repositories for
          the PRs and reviews it surfaces.
        </p>
      </div>
      <UrlField
        label="GitHub URL"
        value={state.org.github_url}
        onChange={(url) => patch({ org: { ...state.org, github_url: url } })}
        placeholder={DEFAULT_GITHUB_URL}
        helpText="github.com for the common case; a *.ghe.com data-residency subdomain or your GitHub Enterprise Server host otherwise."
        invalid={!!error}
      />
    </div>
  )
}

// GitHubAccessStep body — the App/PAT switcher. No Connect button on the PAT
// path (Continue connects); the App path keeps GitHubAppPanel's Register
// launch. The connect error surfaces via the host's shared error line.
export function GitHubAccessStep({ orgId, isLocal, state, patch }: StepContext) {
  const tab = state.githubAccessTab
  return (
    <div className="space-y-3">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        How Triage Factory connects to your organization&rsquo;s GitHub — the identity its bots poll
        repositories and open pull requests under. This is the org-wide connection, not your
        personal GitHub access.
      </p>

      {state.githubReady && (
        <div className="flex items-center gap-2 rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-2.5">
          <div className="h-1.5 w-1.5 shrink-0 rounded-full bg-claim" />
          <span className="text-[12px] text-claim">GitHub connected.</span>
        </div>
      )}

      {/* App (default) vs PAT — the access method switcher. */}
      <div className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5">
        {(['app', 'pat'] as const).map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => patch({ githubAccessTab: t })}
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
          <GitHubAppPanel orgId={orgId} showHeading={false} />
        ) : (
          <p className="text-[12px] italic text-text-tertiary">Resolving your workspace…</p>
        )
      ) : (
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
          showHeading={false}
        />
      )}
    </div>
  )
}
