// The GitHub backbone, split into atomic stack entries (GitHub is the backbone,
// so it's never grouped with the trackers):
//
//   GitHubUrlStep  — the base-URL field. Continue runs the URL-only
//                    reachability probe (no separate "Verify" button); an
//                    unreachable host bounces Continue and turns the box red.
//   GitHubModeStep — the access-method picker: GitHub App vs Personal Access
//                    Token, as two flush side-by-side panels (no box), the
//                    choice its own step in the flow. Lifts the selection into
//                    wizard state (state.githubAccessTab) so the gated config
//                    steps below branch on it.
//   GitHubAppStep / GitHubPatStep — the chosen method's setup, each its own
//                    first-class step gated on the mode (exactly one is visible).
//                    The App step keeps GitHubAppPanel's external Register launch
//                    (a redirect can't fold into Continue); the PAT step has no
//                    separate Connect button — Continue performs the connect.
//
// NOTE: the inner GitHubAppPanel / GitHubAccessGroup boxes are still card-like;
// they get the flush glass overhaul in a follow-up now that they're first-class
// steps. Reuse rule: composes the shared GitHubAccessGroup (PAT-only, base URL
// suppressed) and GitHubAppPanel — no parallel field UIs.

import GitHubAccessGroup from '../settings/GitHubAccessGroup'
import GitHubAppPanel from '../settings/GitHubAppPanel'
import { nextRadioIndex } from '../../lib/rovingRadio'
import { useRef } from 'react'
import { UrlField } from './parts'
import type { StepContext, GitHubAccessMode } from './types'

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

interface ModeCard {
  kind: GitHubAccessMode
  title: string
  detail: string
}

const MODE_CARDS: ModeCard[] = [
  {
    kind: 'app',
    title: 'GitHub App',
    detail: 'Polls under its own bot identity and supports multiple installations.',
  },
  {
    kind: 'pat',
    title: 'Personal access token',
    detail: 'A classic token with repo + read:org scopes — the simpler setup.',
  },
]

// GitHubModeStep body — the access-method picker. Two flush panels side by side
// (a hairline between, no box), the selected one marked by accent text + a thin
// accent bar; the unchosen one stays muted. Arrow keys move the radiogroup. The
// choice rides state.githubAccessTab, which gates the App / PAT config steps.
export function GitHubModeStep({ state, patch }: StepContext) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])
  const selectedIndex = MODE_CARDS.findIndex((c) => c.kind === state.githubAccessTab)
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, MODE_CARDS.length, () => true)
    if (next === null) return
    e.preventDefault()
    patch({ githubAccessTab: MODE_CARDS[next].kind })
    btnRefs.current[next]?.focus()
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          How should the bots connect?
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          How Triage Factory connects to your organization&rsquo;s GitHub — the identity its bots
          poll and open pull requests under. This is the org-wide connection, not your personal
          GitHub access.
        </p>
      </div>
      <div
        role="radiogroup"
        aria-label="GitHub access method"
        onKeyDown={onKeyDown}
        className="grid grid-cols-2 divide-x divide-[var(--color-border-subtle)]"
      >
        {MODE_CARDS.map((card, i) => {
          const selected = state.githubAccessTab === card.kind
          return (
            <button
              key={card.kind}
              ref={(el) => {
                btnRefs.current[i] = el
              }}
              type="button"
              role="radio"
              aria-checked={selected}
              tabIndex={i === Math.max(selectedIndex, 0) ? 0 : -1}
              onClick={() => patch({ githubAccessTab: card.kind })}
              className={`group relative flex flex-col gap-1 px-4 py-3 text-left outline-none first:pl-0 last:pr-0 ${
                i === 0 ? 'pr-5' : 'pl-5'
              }`}
            >
              <span
                className={`text-[14px] font-medium transition-colors ${
                  selected ? 'text-accent' : 'text-text-secondary group-hover:text-text-primary'
                }`}
              >
                {card.title}
              </span>
              <span className="text-[11px] leading-snug text-text-tertiary">{card.detail}</span>
              <span
                className={`absolute -bottom-px left-0 right-0 h-px transition-colors ${
                  selected ? 'bg-accent' : 'bg-transparent'
                } ${i === 0 ? 'right-5' : 'left-5'}`}
              />
            </button>
          )
        })}
      </div>
    </div>
  )
}

// GitHubAppStep body — the GitHub App registration (GitHubAppPanel owns the
// external Register launch + install-status polling). Still card-like inside;
// flush overhaul to follow.
export function GitHubAppStep({ orgId }: StepContext) {
  return orgId ? (
    <GitHubAppPanel orgId={orgId} showHeading={false} />
  ) : (
    <p className="text-[12px] italic text-text-tertiary">Resolving your workspace…</p>
  )
}

// GitHubPatStep body — the PAT fields (+ clone protocol in local). No Connect
// button: the step's Continue performs the connect (steps.tsx). Still card-like
// inside; flush overhaul to follow.
export function GitHubPatStep({ orgId, isLocal, state, patch }: StepContext) {
  return (
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
  )
}
