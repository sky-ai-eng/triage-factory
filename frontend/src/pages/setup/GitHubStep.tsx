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
import GitHubAppImportForm from '../settings/GitHubAppImportForm'
import { GitHubAppInstallView } from '../settings/GitHubAppInstallView'
import { useGitHubAppInstall } from '../../hooks/useGitHubAppInstall'
import { UrlField, ChoiceCards, ReuseCredentialCheckbox } from './parts'
import type { StepContext, GitHubAccessMode, GitHubAppSource, WizardState } from './types'
import { appImportedPatch } from './githubAppImported'

export const DEFAULT_GITHUB_URL = 'https://github.com'

// SwitchNotice is the inline banner the cross-pick steps render when the user
// is changing GitHub access method (not a fresh setup): an amber-tinted note
// explaining what the switch does. Nothing is destroyed by reading it — the
// teardown/cutover only happens on the relevant step's Continue.
function SwitchNotice({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-xl border border-warm/20 bg-warm/[0.08] px-4 py-2.5 text-ui leading-relaxed text-warm dark:text-warm">
      {children}
    </div>
  )
}

// GitHubUrlStep body — just the base-URL field. The reachability probe and the
// base-URL persist are the step's Continue (steps.tsx); `error` turns the box
// red when that probe failed, while the message renders in the host's shared
// error line.
export function GitHubUrlStep({ state, patch, error }: StepContext) {
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          Where does your GitHub live?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
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

// appModeStatus / patModeStatus derive each mode card's "persisted reality"
// status line from wizard state (no new fetches — the install-state loader
// already surfaced these). The App line distinguishes staged (switch pending),
// installed, and registered-not-installed; the PAT line reports a live token.
function appModeStatus(s: WizardState): string | undefined {
  const tag = s.githubAppSlug ? ` (${s.githubAppSlug})` : ''
  if (s.githubAppStaged) return `Registered${tag} — switch pending`
  if (s.githubAppRegistered) {
    return s.githubAppInstalled
      ? `Registered${tag} — installed`
      : `Registered${tag} — not installed yet`
  }
  return undefined
}

function patModeStatus(s: WizardState): string | undefined {
  // A live PAT reads "Connected". During a staged PAT→App switch the PAT is
  // still live, so this correctly shows alongside the App card's "switch
  // pending" — both states are true at once.
  return s.hasGitHubPat ? 'Connected' : undefined
}

// GitHubModeStep body — the access-method picker. Action-on-click (the step is
// selfAdvancing): it opens unselected, and clicking a panel records the choice
// and advances to that method's first config step. Each card carries a status
// line reflecting what's actually persisted, so a returning/mid-switch user
// sees reality rather than two blank options.
export function GitHubModeStep({ state, patch, advance }: StepContext) {
  const choose = (kind: GitHubAccessMode) => {
    patch({ githubAccessTab: kind })
    advance?.()
  }
  const options = [
    {
      kind: 'app' as const,
      title: 'GitHub App',
      detail:
        'Polls under its own durable bot identity, with per-team repository access enforced by GitHub.',
      status: appModeStatus(state),
    },
    {
      kind: 'pat' as const,
      title: 'Personal access token',
      detail: 'A classic token with repo + read:org + user:email scopes — the simpler setup.',
      status: patModeStatus(state),
    },
  ]
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          How should the bots connect?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          How Triage Factory connects to your organization&rsquo;s GitHub — the identity its bots
          poll and open pull requests under. This is the org-wide connection, not your personal
          GitHub access.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="GitHub access method"
        options={options}
        selected={state.githubAccessTab}
        onChoose={choose}
      />
    </div>
  )
}

const SOURCE_CARDS: { kind: GitHubAppSource; title: string; detail: string }[] = [
  {
    kind: 'create',
    title: 'Create a new App',
    detail: 'Triage Factory registers a new GitHub App for you via the manifest flow.',
  },
  {
    kind: 'existing',
    title: 'Connect an existing App',
    detail: 'You have an App ID and private key — bring your own, no org-owner consent needed.',
  },
]

// GitHubAppSourcePicker is the shared create-vs-connect chooser — the ChoiceCards
// plus heading — reused by the wizard's source step and the Settings switch flow.
// onChoose receives the kind directly so callers route on the argument (not a
// not-yet-flushed state read).
export function GitHubAppSourcePicker({
  selected,
  onChoose,
}: {
  selected: GitHubAppSource | null
  onChoose: (kind: GitHubAppSource) => void
}) {
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          New App or one you already have?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          Register a fresh GitHub App, or connect one that already exists — useful when only a
          platform team may create org Apps, or you&rsquo;re reusing an App across deployments.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="GitHub App source"
        options={SOURCE_CARDS}
        selected={selected}
        onChoose={onChoose}
      />
    </div>
  )
}

// GitHubAppSourceStep body — App only: create a new App vs connect an existing
// one. Action-on-click (selfAdvancing); the choice rides state.githubAppSource,
// gating the account-type/register steps (create) vs the import step (existing).
export function GitHubAppSourceStep({ state, patch, advance }: StepContext) {
  return (
    <GitHubAppSourcePicker
      selected={state.githubAppSource}
      onChoose={(kind) => {
        patch({ githubAppSource: kind })
        advance?.()
      }}
    />
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
// registration step. When the org already has a live PAT (a PAT→App switch),
// it leads with the staged-switch notice so the user knows the PAT stays the
// live credential until the App is installed and cut over.
export function GitHubAccountTypeStep({ state, patch, advance }: StepContext) {
  const choose = (kind: 'user' | 'org') => {
    patch({ githubAppOwnerType: kind })
    advance?.()
  }
  // hasGitHubPat with no live App yet ⇒ this is a switch, not a fresh setup.
  const switching = state.hasGitHubPat && !state.githubAppRegistered
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          Who owns the GitHub App?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          Where the App is registered — your personal account, or an organization you administer.
        </p>
      </div>
      {switching && (
        <SwitchNotice>
          Your personal access token stays active until the App is installed, then it&rsquo;s
          removed.
        </SwitchNotice>
      )}
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
      initialStatus={state.githubAppStatus ?? undefined}
      returnTo={returnTo}
    />
  ) : (
    <p className="text-ui italic text-ink-3">Resolving your workspace…</p>
  )
}

// GitHubAppImportStep body — App + existing source: the bring-your-own-App
// import form. The form owns its own submit (the step is selfAdvancing — no
// wizard Continue); on a successful import it patches the same state the register
// path patches (via appImportedPatch) and advances to the install step.
export function GitHubAppImportStep({ orgId, isLocal, patch, advance }: StepContext) {
  return orgId ? (
    <GitHubAppImportForm
      orgId={orgId}
      isLocal={isLocal}
      onImported={(result) => {
        patch(appImportedPatch(result))
        advance?.()
      }}
    />
  ) : (
    <p className="text-ui italic text-ink-3">Resolving your workspace…</p>
  )
}

// GitHubAppInstallStep body — the gated "Install the App" step. The App is
// registered by the prior step; here the user installs it on GitHub (the
// deep-link + the accounts it's already installed on come from
// useGitHubAppInstall, which refetches on focus so returning from the install
// tab updates the list). The step's Continue (steps.tsx) is refresh-then-verify
// — it reconciles the installation mirror and only advances once GitHub reports
// an installation — so this body carries no action of its own.
export function GitHubAppInstallStep({ orgId, state }: StepContext) {
  // Hook called unconditionally (stable hook order), then branch on orgId.
  const { status, installUrl } = useGitHubAppInstall(orgId)
  if (!orgId) {
    return <p className="text-ui italic text-ink-3">Resolving your workspace…</p>
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Install the App</h2>
        <p className="text-body leading-relaxed text-ink-3">
          The App is registered, but GitHub only grants repository access once it&rsquo;s installed.
          Install it on your account or organization, choose all or selected repositories, then come
          back and continue — we&rsquo;ll verify the installation before moving on.
        </p>
      </div>
      {state.githubAppStaged && (
        <SwitchNotice>
          When you continue, Triage Factory verifies the installation, switches over to the App, and
          removes your personal access token.
        </SwitchNotice>
      )}
      <GitHubAppInstallView installations={status?.installations ?? []} installUrl={installUrl} />
    </div>
  )
}

// GitHubPatStep body — the PAT token field alone (clone protocol is its own
// step now, so showCloneProtocol is false). No Connect button: the step's
// Continue performs the connect (steps.tsx). When an App is registered, the
// step is a cross-pick — Continue tears the App down and switches to the typed
// PAT — so it leads with the teardown warning. Nothing happens until Continue.
export function GitHubPatStep({ orgId, isLocal, state, patch }: StepContext) {
  return (
    <div className="space-y-5">
      {state.githubAppRegistered && (
        <SwitchNotice>
          {state.githubAppStaged && state.hasGitHubPat ? (
            <>
              You have a staged GitHub App
              {state.githubAppSlug ? ` (${state.githubAppSlug})` : ''}. Continue to discard it and
              keep your current token — or enter a new token to replace it. The App stays on GitHub
              and can be deleted there.
            </>
          ) : (
            <>
              You&rsquo;ve registered a GitHub App
              {state.githubAppSlug ? ` (${state.githubAppSlug})` : ''}. Entering a personal access
              token will remove that registration — the App itself stays on GitHub and can be
              deleted there.
            </>
          )}
        </SwitchNotice>
      )}
      <GitHubAccessGroup
        value={{
          github_url: state.org.github_url,
          github_pat: state.org.github_pat,
          github_clone_protocol: state.org.github_clone_protocol,
        }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        // A staged App keeps the live PAT, so "leave blank to keep current" still
        // holds (blank → discard the staged App, PAT stays). A live App has no
        // PAT (XOR), so hasGitHubPat is already false there — no special-casing
        // the App-registered flag is needed.
        hasToken={state.hasGitHubPat}
        isLocal={isLocal}
        orgId={orgId}
        showAppPanel={false}
        showBaseUrl={false}
        showHeading={false}
        showCloneProtocol={false}
        bare
      />
      {/* Local-mode only: reuse the org PAT as the operator's own GitHub identity
          (PAT_2) so they don't paste it again on the User step. Hidden during an
          App→PAT cross-pick (a re-config teardown, where reuse doesn't apply) and
          once GitHub is already connected (nothing fresh to reuse). */}
      {isLocal && !state.githubAppRegistered && !state.githubReady && (
        <ReuseCredentialCheckbox
          checked={state.duplicateGitHubToUser}
          onChange={(v) => patch({ duplicateGitHubToUser: v })}
          label="Also use this token as my own GitHub identity"
          hint="Saves re-entering it on the “Your GitHub identity” step. Only your username is read — the token isn’t stored as your identity."
        />
      )}
    </div>
  )
}

const CLONE_CARDS: { kind: 'ssh' | 'https'; title: string; detail: string }[] = [
  {
    kind: 'ssh',
    title: 'SSH',
    detail: 'Uses your own key and agent. Choose this if your host restricts Git over HTTPS.',
  },
  {
    kind: 'https',
    title: 'HTTPS',
    detail: 'Uses your token — no SSH setup, and pushes are checked and recorded.',
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
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          How should repos be cloned?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          Sets the transport for every Git operation Triage Factory and its agents make — cloning,
          fetching and pushing. The GitHub API connection is HTTPS either way.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="Clone protocol"
        options={CLONE_CARDS}
        selected={state.org.github_clone_protocol}
        onChoose={choose}
      />
      {/* Shown before the choice, not after it: this is what the choice costs,
          and in the setup wizard picking an option advances the step. */}
      <p className="text-body leading-relaxed text-ink-3">
        Base-branch protection and push recording ride the HTTPS identity, so they do not apply on
        SSH. There, a run&rsquo;s pushes are checked on this machine before they leave, and{' '}
        <code>git push --no-verify</code> skips that check.
      </p>
    </div>
  )
}
