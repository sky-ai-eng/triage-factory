// The GitHub backbone, split into atomic stack entries (GitHub is the backbone,
// so it's never grouped with the trackers):
//
//   GitHubUrlStep         — the base-URL field. Continue runs the URL-only
//                           reachability probe; an unreachable host bounces
//                           Continue and turns the box red.
//   GitHubModeStep        — the access-method picker (App vs PAT), a flush
//                           two-panel ChoiceCards, action-on-click.
//   GitHubAccountTypeStep — App only: which account the App is registered under
//                           (Personal vs Organization), a ChoiceCards before the
//                           registration step. Rides state.githubAppOwnerType.
//   GitHubAppStep         — App only: GitHubAppPanel's external Register launch
//                           (a redirect can't fold into Continue), owner type
//                           controlled from the prior step.
//   GitHubPatStep         — PAT only: the token field. Continue performs the
//                           connect (no separate button).
//   GitHubCloneStep       — PAT + local only: SSH vs HTTPS clone protocol, a
//                           ChoiceCards after the token step.
//
// Reuse rule: composes the shared GitHubAccessGroup (token-only, base URL +
// clone protocol suppressed) and GitHubAppPanel (controlled owner type) — no
// parallel field UIs — and the shared ChoiceCards picker.

import GitHubAccessGroup from '../settings/GitHubAccessGroup'
import GitHubAppPanel from '../settings/GitHubAppPanel'
import { GitHubAppInstallView } from '../settings/GitHubAppInstallView'
import { useGitHubAppInstall } from '../../hooks/useGitHubAppInstall'
import { UrlField, ChoiceCards } from './parts'
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

const MODE_CARDS: { kind: GitHubAccessMode; title: string; detail: string }[] = [
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

// GitHubModeStep body — the access-method picker. Action-on-click (the step is
// selfAdvancing): it opens unselected, and clicking a panel records the choice
// and advances to that method's first config step.
export function GitHubModeStep({ state, patch, advance }: StepContext) {
  const choose = (kind: GitHubAccessMode) => {
    patch({ githubAccessTab: kind })
    advance?.()
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
      <ChoiceCards
        ariaLabel="GitHub access method"
        options={MODE_CARDS}
        selected={state.githubAccessTab}
        onChoose={choose}
      />
    </div>
  )
}

const ACCOUNT_CARDS: { kind: 'user' | 'org'; title: string; detail: string }[] = [
  {
    kind: 'user',
    title: 'Personal',
    detail: 'Register the App under your personal GitHub account.',
  },
  {
    kind: 'org',
    title: 'Organization',
    detail: 'Register the App under a GitHub organization you administer.',
  },
]

// GitHubAccountTypeStep body — App only: which account the App registers under.
// Action-on-click; the choice rides state.githubAppOwnerType into the
// registration step.
export function GitHubAccountTypeStep({ state, patch, advance }: StepContext) {
  const choose = (kind: 'user' | 'org') => {
    patch({ githubAppOwnerType: kind })
    advance?.()
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Who owns the GitHub App?
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Where the App is registered — your personal account, or an organization you administer.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="App account type"
        options={ACCOUNT_CARDS}
        selected={state.githubAppOwnerType}
        onChoose={choose}
      />
    </div>
  )
}

// GitHubAppStep body — the GitHub App registration (GitHubAppPanel owns the
// external Register launch). The owner type is controlled from the prior
// account-type step, so the panel hides its own toggle. `returnTo` is passed by
// the render site, not carried on StepContext: the wizard passes 'setup' (so the
// post-registration redirect lands back on /setup at the install step), while
// the Settings reuse passes 'settings'. Installation itself is the NEXT step
// (GitHubAppInstallStep) — the panel no longer owns an install affordance.
export function GitHubAppStep({
  orgId,
  state,
  returnTo,
}: StepContext & { returnTo: 'setup' | 'settings' }) {
  return orgId ? (
    <GitHubAppPanel
      orgId={orgId}
      showHeading={false}
      bare
      ownerType={state.githubAppOwnerType}
      returnTo={returnTo}
    />
  ) : (
    <p className="text-[12px] italic text-text-tertiary">Resolving your workspace…</p>
  )
}

// GitHubAppInstallStep body — the gated "Install the App" step. The App is
// registered by the prior step; here the user installs it on GitHub (the
// deep-link + the accounts it's already installed on come from
// useGitHubAppInstall, which refetches on focus so returning from the install
// tab updates the list). The step's Continue (steps.tsx) is refresh-then-verify
// — it reconciles the installation mirror and only advances once GitHub reports
// an installation — so this body carries no action of its own.
export function GitHubAppInstallStep({ orgId }: StepContext) {
  // Hook called unconditionally (stable hook order), then branch on orgId.
  const { status, installUrl } = useGitHubAppInstall(orgId)
  if (!orgId) {
    return <p className="text-[12px] italic text-text-tertiary">Resolving your workspace…</p>
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Install the App
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          The App is registered, but GitHub only grants repository access once it&rsquo;s installed.
          Install it on your account or organization, choose all or selected repositories, then come
          back and continue — we&rsquo;ll verify the installation before moving on.
        </p>
      </div>
      <GitHubAppInstallView installations={status?.installations ?? []} installUrl={installUrl} />
    </div>
  )
}

// GitHubPatStep body — the PAT token field alone (clone protocol is its own
// step now, so showCloneProtocol is false). No Connect button: the step's
// Continue performs the connect (steps.tsx).
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
      showCloneProtocol={false}
      bare
    />
  )
}

const CLONE_CARDS: { kind: 'ssh' | 'https'; title: string; detail: string }[] = [
  {
    kind: 'ssh',
    title: 'SSH',
    detail: 'Clone over SSH using your key (an SSH agent must be configured).',
  },
  {
    kind: 'https',
    title: 'HTTPS',
    detail: 'Clone over HTTPS using your token — no SSH setup needed.',
  },
]

// GitHubCloneStep body — PAT + local only: how repos clone to the machine.
// Action-on-click; the step's persist saves the org form with the new protocol.
export function GitHubCloneStep({ state, patch, advance }: StepContext) {
  const choose = (kind: 'ssh' | 'https') => {
    patch({ org: { ...state.org, github_clone_protocol: kind } })
    advance?.()
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          How should repos be cloned?
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Only affects how Triage Factory clones repos to this machine — not the API connection.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="Clone protocol"
        options={CLONE_CARDS}
        selected={state.org.github_clone_protocol}
        onChoose={choose}
      />
    </div>
  )
}
