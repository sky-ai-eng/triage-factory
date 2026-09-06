// GitHubAccessControl is the body of Settings' single "GitHub access" section —
// the post-TFAC-328 replacement for the old independently-editable App/PAT
// credential sections. GitHub access is either/or per org, so this surface shows
// the LIVE mode plainly and offers a guided switch to the other mode (with an
// inform-only reachability diff and a confirm step), plus the staged-switch
// banner when a PAT→App switch is registered but not yet cut over.
//
// A PAT org gets one more affordance: replace the token in place. Rotation is
// hygiene, not a mode change — expiry, a revocation, an offboarding — and the
// only alternatives were switching to an App and back, clearing every token in
// the danger zone, or calling the API by hand. It reuses the switch flow's two
// screens (token entry → validated login + reachability diff) because a rotation
// can darken repositories exactly like a switch can: a replacement token with
// narrower reach is the offboarding case, not an edge case. The commit is the
// credential resource's own PUT, which validates live and leaves the existing
// credential in place on a bad token.
//
// It reuses the same step components the wizard composes — GitHubAccountTypeStep
// and GitHubAppStep (GitHubAppPanel, returnTo="settings"), and the shared
// install view (GitHubAppInstallView + useGitHubAppInstall) — so the switch
// reads as the same material as /setup. The diff/confirm/success screens are
// the Settings-only additions (the wizard is pre-repos, so it has no diff).
//
// State is read off the StepContext OrgSettings already owns (githubApp* fields
// seeded by its load); committing a switch calls `reload` so OrgSettings
// re-derives the live mode, the section summary, and the clone-protocol gate.
//
// The registration leg redirects to GitHub (the manifest flow), so the modal
// flow can't span it: after registering, the App is STAGED and the user lands
// back here on the staged banner, whose "Finish" resumes the stepper at the
// install/diff step. That's by design, not a gap.
//
// Three credential classes render here, and the affordances are per class:
//
//   - A workspace with its OWN App: its slug, its installations (with
//     suspension and grant width), both grant findings, and the switch to a
//     token.
//   - A workspace on the DEPLOYMENT's App (multi mode): no App of its own to
//     show and none to register or import — the same treatment the Atlassian
//     card gives "using the deployment app". What it has is the accounts it
//     bound, each with its own Disconnect, the findings, Connect for another
//     account, and Disconnect for the whole workspace: the way out for one that
//     wants its own App or a token instead. The grant stays on GitHub either
//     way; the copy says so and links there.
//   - A workspace with a TOKEN, or nothing: neither finding — a token's reach
//     is not a grant TF holds — and, with nothing bound, every way in at once:
//     Connect (only when the deployment offers an App; a default, never the
//     only option), register, import, or a token.

import { useState } from 'react'
import { ExternalLink } from 'lucide-react'
import { toast } from '../../../components/Toast/toastStore'
import { isHttpUrl } from '../../../lib/reachability'
import { GitHubAccountTypeStep, GitHubAppSourcePicker, GitHubAppStep } from '../../setup/GitHubStep'
import GitHubAppImportForm from '../GitHubAppImportForm'
import ConnectInstalledAccount from '../ConnectInstalledAccount'
import { appImportedPatch } from '../../setup/githubAppImported'
import { GitHubAppInstallView } from '../GitHubAppInstallView'
import GitHubWebhookHealthNotice from '../GitHubWebhookHealthNotice'
import { useGitHubAppInstall } from '../../../hooks/useGitHubAppInstall'
import { useGrantFindings } from '../../../hooks/useGrantFindings'
import { GitHubGrantFindings, GitHubInstallationList } from './GitHubGrantPanel'
import {
  cutoverPreflight,
  cutoverToApp,
  discardStagedApp,
  disconnectManagedGitHub,
  disconnectOwnApp,
  disconnectManagedInstallation,
  getGitHubAppStatus,
  patPreflight,
  refreshGitHubAppInstallations,
  startManagedGitHubConnect,
  switchToPat,
  type AccessDiff,
  type GitHubAppInstallation,
} from '../../../lib/githubApp'
import { connectGitHubPAT } from '../orgCredentials'
import type { StepContext } from '../../setup/types'

// The guided-switch state machine. `idle` shows the live mode + the switch
// affordance (or the staged banner); the rest are the stepper screens, split by
// direction, with the two `rotate-*` screens serving the same-mode PAT
// replacement — and, for a workspace with nothing bound, the first token bind,
// which is the same two screens (validate, show the reach, commit through the
// credential's PUT) under different words. The register screen is transient —
// clicking Register redirects away, and the user returns to `idle` on the
// staged banner.
type Phase =
  | { kind: 'idle' }
  | { kind: 'rotate-token' }
  | { kind: 'rotate-diff'; diff: AccessDiff; login: string; pat: string }
  | { kind: 'to-app-source' }
  | { kind: 'to-app-account' }
  | { kind: 'to-app-register' }
  | { kind: 'to-app-import' }
  | { kind: 'to-app-install' }
  | { kind: 'to-app-diff'; diff: AccessDiff }
  | { kind: 'to-pat-token' }
  | { kind: 'to-pat-diff'; diff: AccessDiff; login: string; pat: string }
  | { kind: 'to-pat-success'; settingsUrl: string; login: string }

// What an App buys, shared by the two branches of the no-live-App paragraph
// below — they differ only in the lead-in verb ("Switch to a GitHub App to …"
// when a PAT is connected, "Register a GitHub App to …" when nothing is). One
// fragment rather than one copy per branch: a sentence duplicated across a
// ternary is one edit away from two divergent claims about what an App buys,
// and the claim about GitHub enforcing the scope is the load-bearing half.
const appPitch = (
  <>
    poll under a bot identity of its own — one that doesn&rsquo;t leave with the person who set it
    up — and to have GitHub itself scope each team&rsquo;s access to the repositories that team
    tracks
  </>
)

export default function GitHubAccessControl({
  ctx,
  reload,
}: {
  // The StepContext OrgSettings owns — its live draft (with the githubApp*
  // fields its load seeded) + patch, reused so the account-type/register step
  // bodies render identically to /setup.
  ctx: StepContext
  // Re-run OrgSettings' load after a committed switch, so the live mode, the
  // section summary, and the clone-protocol gate all re-derive.
  reload: () => void
}) {
  const orgId = ctx.orgId
  const s = ctx.state
  const registered = s.githubAppRegistered
  const staged = s.githubAppStaged
  const liveApp = registered && !staged
  const slug = s.githubAppSlug
  // The live token is env-supplied, so this workspace can report GitHub access
  // but not change it (see the idle branch below). Ranked under the App states:
  // a registered App is its own credential tier, and the overlay only shadows
  // the PAT the App path doesn't read.
  const envPat = s.githubPatEnvProvided

  const [phase, setPhase] = useState<Phase>({ kind: 'idle' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Install state for the to-app-install screen (and to read the live
  // installation count for the staged banner's resume target). Refetches on
  // focus, so returning from the GitHub install tab updates the list.
  const {
    status: installStatus,
    setStatus: setInstallStatus,
    installUrl,
  } = useGitHubAppInstall(orgId)
  const installations = installStatus?.installations ?? []
  const installCount = installStatus?.installations.length ?? s.githubAppInstallCount
  // The deployment's App is the credential: no App row of the org's own, only
  // the installations it has bound. Seeded by the loader; the live status read
  // refines it so a bind completed in another tab is reflected on focus.
  const managed = installStatus?.using_deployment_default ?? s.githubAppManaged
  // Whether there is a deployment App to bind at all — the one input that
  // decides whether the empty state offers Connect. Never seeded: it is a
  // fact about the deployment, and until the status read answers, the empty
  // state offers the three paths every deployment has.
  const deploymentAppAvailable = installStatus?.deployment_app_available ?? false
  // The grant findings, for the two App classes only. Keyed on the bound set
  // so a disconnect reloads them against the grant that remains.
  const findings = useGrantFindings(
    orgId,
    liveApp || managed,
    installations.map((i) => i.installation_id).join(','),
  )

  const reset = () => {
    setPhase({ kind: 'idle' })
    setBusy(false)
    setError(null)
  }

  // ── Discard a staged switch ──
  const discard = async () => {
    if (!orgId || busy) return
    if (!confirm('Discard the staged GitHub App? Your personal access token stays active.')) return
    setBusy(true)
    setError(null)
    try {
      await discardStagedApp(orgId)
      toast.success('Staged GitHub App discarded')
      // The PAT stays live; optimistically clear the staged App so the banner
      // doesn't linger during the async reload.
      ctx.patch({
        githubAppRegistered: false,
        githubAppStaged: false,
        githubAppInstalled: false,
        githubAppInstallCount: 0,
        githubAppSlug: '',
      })
      reset()
      reload()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── PAT→App: install → preflight → diff ──
  // Both the install step's Continue and the staged banner's "Finish" land
  // here: verify ≥1 installation, then fetch the cutover reachability diff.
  const toAppPreflight = async () => {
    if (!orgId || busy) return
    setBusy(true)
    setError(null)
    try {
      // Reconcile first so "installed?" reflects GitHub now (local mode never
      // gets the webhook); a zero count means nothing's installed yet.
      const fresh = await refreshGitHubAppInstallations(orgId)
      if (fresh.installations.length === 0) {
        setError(
          "We can't see the App installed on any account yet — install it on GitHub, then try again.",
        )
        setBusy(false)
        return
      }
      const diff = await cutoverPreflight(orgId)
      setPhase({ kind: 'to-app-diff', diff })
      setBusy(false)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── PAT→App: commit the cutover ──
  const commitCutover = async () => {
    if (!orgId || busy) return
    setBusy(true)
    setError(null)
    try {
      await cutoverToApp(orgId)
      toast.success('Switched to the GitHub App')
      // Optimistically flip the draft to live-App so the idle view doesn't
      // flash the staged banner during the async reload; reload confirms it.
      ctx.patch({ githubAppStaged: false, hasGitHubPat: false })
      reset()
      reload()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── App→PAT: validate the token + fetch the diff ──
  const toPatPreflight = async (pat: string) => {
    if (!orgId || busy) return
    setBusy(true)
    setError(null)
    try {
      const pre = await patPreflight(orgId, pat)
      setPhase({ kind: 'to-pat-diff', diff: pre, login: pre.login, pat })
      setBusy(false)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── PAT→PAT: validate the replacement token + fetch the diff ──
  // The same preflight the App→PAT switch runs — it validates against the org's
  // resolved host and enumerates the token's reach, storing nothing, so a bad
  // token fails here with the live credential untouched.
  const rotatePreflight = async (pat: string) => {
    if (!orgId || busy) return
    setBusy(true)
    setError(null)
    try {
      const pre = await patPreflight(orgId, pat)
      setPhase({ kind: 'rotate-diff', diff: pre, login: pre.login, pat })
      setBusy(false)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── PAT→PAT: commit the replacement ──
  // The credential resource's PUT re-validates and swaps the stored token in one
  // transaction; nothing is destroyed if it 422s, so a failure leaves the org on
  // the token it was already using.
  const commitRotate = async (pat: string, login: string) => {
    if (!orgId || busy) return
    setBusy(true)
    setError(null)
    const res = await connectGitHubPAT(orgId, pat)
    if (!res.ok) {
      setError(res.error)
      setBusy(false)
      return
    }
    const bound = res.login || login
    const verb = s.hasGitHubPat ? 'GitHub token replaced' : 'Connected with a personal access token'
    toast.success(bound ? `${verb} — as @${bound}` : verb)
    // Optimistic so the idle view names the new account immediately; reload
    // confirms it from the server.
    ctx.patch({ hasGitHubPat: true, githubPatLogin: bound })
    reset()
    reload()
  }

  // ── App→PAT: commit the teardown ──
  const commitSwitchToPat = async (pat: string, login: string) => {
    if (!orgId || busy) return
    setBusy(true)
    setError(null)
    try {
      const res = await switchToPat(orgId, pat)
      // Success screen first (the delete-on-GitHub + re-identify guidance); the
      // mode header flips when the user dismisses it and we reload. Gate the URL
      // to http(s) before it becomes an <a href> — defense in depth (the backend
      // derives it from the validated base URL, but a javascript: href would
      // still execute despite rel=noopener), mirroring getGitHubAppInstallURL.
      const settingsUrl = isHttpUrl(res.github_app_settings_url) ? res.github_app_settings_url : ''
      setPhase({ kind: 'to-pat-success', settingsUrl, login })
      setBusy(false)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── Deployment App: disconnect one account, or the whole workspace ──
  // The two verbs behind the managed panel's only destructive affordances.
  // Nothing is uninstalled on GitHub — the installation persists there
  // unbound — so the confirm says so. Afterwards the live status is re-read
  // and folded into the install hook, so the panel re-renders off the server's
  // answer: after the last account goes, that is the no-credential empty
  // state, with Connect still offered beside the other three paths.
  const disconnectManaged = async (only?: GitHubAppInstallation) => {
    if (!orgId || busy) return
    const question = only
      ? `Disconnect @${only.account_login} from this workspace? The App stays installed on GitHub; Triage Factory just stops using it here.`
      : 'Disconnect this workspace from the deployment’s GitHub App? Every connected account is released. The App stays installed on GitHub.'
    if (!confirm(question)) return
    setBusy(true)
    setError(null)
    try {
      if (only) {
        await disconnectManagedInstallation(orgId, only.installation_id)
      } else {
        await disconnectManagedGitHub(orgId)
      }
      const fresh = await getGitHubAppStatus(orgId)
      setInstallStatus(fresh)
      const n = fresh.installations.length
      ctx.patch({
        githubAppManaged: fresh.using_deployment_default,
        githubAppInstalled: n > 0,
        githubAppInstallCount: n,
      })
      toast.success(
        only
          ? `Disconnected @${only.account_login}`
          : 'Disconnected from the deployment’s GitHub App',
      )
      setBusy(false)
      reload()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ── Own App: switch to the deployment's ──
  // The teardown verb followed straight away by the deployment App's Connect,
  // which is a navigation to GitHub's install page. The workspace holds no
  // credential for the length of that trip; the app shell treats that as
  // setup left unfinished and routes an admin who comes back without an
  // account connected into the setup wizard, which is why the confirm says
  // so. There is deliberately no bare disconnect here: a teardown with no
  // replacement lands in exactly that wizard, and the only reason to want one
  // is to connect something else — which is what this is.
  const switchToDeploymentApp = async () => {
    if (!orgId || busy) return
    if (
      !confirm(
        `Switch this workspace to the deployment’s GitHub App? ${slug ? `The ${slug} App` : 'Your App'} is disconnected first (it stays registered on GitHub), then you pick a GitHub account to connect on GitHub. If you leave GitHub’s page without connecting one, this workspace has no GitHub access and you’ll be taken back to setup.`,
      )
    )
      return
    setBusy(true)
    setError(null)
    try {
      await disconnectOwnApp(orgId)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
      return
    }
    // The draft no longer describes an App, so a return from GitHub's page
    // without a bind renders the empty state rather than a card for a
    // registration that is gone.
    ctx.patch({
      githubAppRegistered: false,
      githubAppStaged: false,
      githubAppInstalled: false,
      githubAppInstallCount: 0,
      githubAppSlug: '',
      hasGitHubPat: false,
      githubPatLogin: '',
    })
    await connectManaged()
  }

  // ── Deployment App: Connect ──
  // Starts the install leg and navigates to GitHub. A refusal lands inline —
  // the start is a fetch now, so there is no page from the server to show it.
  const connectManaged = async () => {
    if (!orgId) return
    setBusy(true)
    setError(null)
    try {
      await startManagedGitHubConnect(orgId)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ─────────────────────────────── render ───────────────────────────────

  // rotate-token — enter the replacement token; Continue validates it and
  // fetches the diff. Nothing has changed yet at this point.
  if (phase.kind === 'rotate-token') {
    return (
      <Frame onCancel={reset} busy={busy} cancelLabel="Cancel">
        <TokenScreen
          title={
            s.hasGitHubPat
              ? 'Replace your personal access token'
              : 'Connect with a personal access token'
          }
          detail={
            s.hasGitHubPat ? (
              <>
                Enter the new token, with <code className="text-ink-2">repo</code>,{' '}
                <code className="text-ink-2">read:org</code>, and{' '}
                <code className="text-ink-2">user:email</code> scopes. We&rsquo;ll validate it and
                show what it can reach before your current token is replaced.
              </>
            ) : (
              <>
                Enter a token with <code className="text-ink-2">repo</code>,{' '}
                <code className="text-ink-2">read:org</code>, and{' '}
                <code className="text-ink-2">user:email</code> scopes. We&rsquo;ll validate it and
                show what it can reach before anything is stored.
              </>
            )
          }
          busy={busy}
          error={error}
          onSubmit={(pat) => void rotatePreflight(pat)}
        />
      </Frame>
    )
  }

  // rotate-diff — the validated login + what the replacement can reach, with the
  // same acknowledgment the switch flows require when repositories would go
  // dark. The swap happens on confirm.
  if (phase.kind === 'rotate-diff') {
    return (
      <Frame onCancel={reset} busy={busy} cancelLabel="Cancel">
        <AccessDiffScreen
          diff={phase.diff}
          login={phase.login}
          action={s.hasGitHubPat ? 'replacement' : 'connection'}
          confirmLabel={s.hasGitHubPat ? 'Replace token' : 'Connect'}
          busy={busy}
          error={error}
          onConfirm={() => void commitRotate(phase.pat, phase.login)}
        />
      </Frame>
    )
  }

  // to-app-source — the create-vs-connect fork (same chooser the wizard uses).
  // Create continues to the account-type/register path; connect goes to the
  // import form. onChoose routes on the argument, and patches ctx so a later
  // cancel reads a coherent source.
  if (phase.kind === 'to-app-source') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <GitHubAppSourcePicker
          selected={s.githubAppSource}
          onChoose={(kind) => {
            ctx.patch({ githubAppSource: kind })
            setPhase(kind === 'existing' ? { kind: 'to-app-import' } : { kind: 'to-app-account' })
          }}
        />
      </Frame>
    )
  }

  // to-app-import — the bring-your-own-App import form. On success the App is
  // persisted (staged if a PAT is live, else immediately active): a staged import
  // continues to install → diff → cutover; a fresh active import (no PAT) is the
  // live credential already, so reload to the live-App view.
  if (phase.kind === 'to-app-import' && orgId) {
    return (
      <Frame onCancel={reset} busy={busy}>
        <div className="space-y-1.5">
          <h3 className="text-column font-medium text-ink-1">Connect an existing App</h3>
        </div>
        <GitHubAppImportForm
          orgId={orgId}
          isLocal={ctx.isLocal}
          onImported={(result) => {
            ctx.patch(appImportedPatch(result))
            if (result.app && !result.app.active) {
              setPhase({ kind: 'to-app-install' })
            } else {
              toast.success('Connected your GitHub App')
              reset()
              reload()
            }
          }}
        />
      </Frame>
    )
  }

  // to-app-account — reuse the wizard's account-type picker; choosing advances
  // to the register step.
  if (phase.kind === 'to-app-account') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <GitHubAccountTypeStep {...ctx} advance={() => setPhase({ kind: 'to-app-register' })} />
      </Frame>
    )
  }

  // to-app-register — reuse GitHubAppPanel (returnTo="settings"); Register
  // redirects to GitHub and the user returns on the staged banner.
  if (phase.kind === 'to-app-register') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <p className="text-body leading-relaxed text-ink-3">
          Register the GitHub App. Your personal access token stays the live credential until you
          install the App and switch over — nothing changes yet.
        </p>
        <GitHubAppStep {...ctx} returnTo="settings" />
      </Frame>
    )
  }

  // to-app-install — install the App, then Continue runs the reachability diff.
  if (phase.kind === 'to-app-install') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <div className="space-y-1.5">
          <h3 className="text-column font-medium text-ink-1">Install the App</h3>
          <p className="text-body leading-relaxed text-ink-3">
            GitHub only grants repository access once the App is installed. Install it on your
            account or organization, then continue to review the switch.
          </p>
        </div>
        <GitHubAppInstallView
          installations={installStatus?.installations ?? []}
          installUrl={installUrl}
        />
        {error && <p className="text-ui text-alarm">{error}</p>}
        {/* Continue is NOT gated on installCount: useGitHubAppInstall swallows
            read failures and can sit at a stale/null status, and toAppPreflight
            runs the authoritative refresh itself (showing its own inline error
            when nothing's installed) — disabling here would lock the user out of
            the very discovery that unsticks them. */}
        <Actions confirmLabel="Continue" onConfirm={() => void toAppPreflight()} busy={busy} />
      </Frame>
    )
  }

  // to-app-diff — the cutover reachability diff + confirm.
  if (phase.kind === 'to-app-diff') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <AccessDiffScreen
          diff={phase.diff}
          confirmLabel="Switch to the GitHub App"
          busy={busy}
          error={error}
          onConfirm={() => void commitCutover()}
        />
      </Frame>
    )
  }

  // to-pat-token — enter the PAT; Continue validates it and fetches the diff.
  if (phase.kind === 'to-pat-token') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <TokenScreen busy={busy} error={error} onSubmit={(pat) => void toPatPreflight(pat)} />
      </Frame>
    )
  }

  // to-pat-diff — the validated login + reachability diff + confirm.
  if (phase.kind === 'to-pat-diff') {
    return (
      <Frame onCancel={reset} busy={busy}>
        <AccessDiffScreen
          diff={phase.diff}
          login={phase.login}
          confirmLabel="Switch to a personal access token"
          busy={busy}
          error={error}
          onConfirm={() => void commitSwitchToPat(phase.pat, phase.login)}
        />
      </Frame>
    )
  }

  // to-pat-success — the App is torn down locally; guide deleting it on GitHub
  // and flag the identity-capture downgrade.
  if (phase.kind === 'to-pat-success') {
    return (
      <div className="space-y-4">
        <div className="rounded-xl border border-line-1 bg-tint-2 px-4 py-3">
          <p className="text-body font-medium text-ink-2">Switched to a personal access token</p>
        </div>
        <p className="text-body leading-relaxed text-ink-2">
          The GitHub App still exists on GitHub — delete it there if you no longer need it:
          {phase.settingsUrl ? (
            <>
              {' '}
              <a
                href={phase.settingsUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-warm hover:underline"
              >
                App settings on GitHub
                <ExternalLink size={12} />
              </a>
              .
            </>
          ) : (
            ' open your GitHub App’s settings page.'
          )}
        </p>
        <p className="text-body leading-relaxed text-ink-3">
          Team members who need to (re)connect their GitHub identity will be asked for a personal
          token instead of one-click OAuth.
        </p>
        <div className="flex justify-end">
          <button
            type="button"
            onClick={() => {
              // Optimistically flip the draft to PAT mode so the idle view
              // doesn't flash "App connected" during the async reload.
              ctx.patch({
                githubAppRegistered: false,
                githubAppStaged: false,
                githubAppInstalled: false,
                githubAppInstallCount: 0,
                githubAppSlug: '',
                hasGitHubPat: true,
                githubPatLogin: phase.login,
              })
              reset()
              reload()
            }}
            className="rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90"
          >
            Done
          </button>
        </div>
      </div>
    )
  }

  // idle — the live-mode summary + the staged banner / switch affordance.
  return (
    <div className="space-y-4">
      {staged ? (
        <StagedBanner
          slug={slug}
          busy={busy}
          error={error}
          onFinish={() =>
            // Resume at the right step: straight to the diff once something's
            // installed, otherwise the install step first.
            installCount > 0 ? void toAppPreflight() : setPhase({ kind: 'to-app-install' })
          }
          onDiscard={() => void discard()}
        />
      ) : liveApp ? (
        <>
          <p className="text-body leading-relaxed text-ink-3">
            Triage Factory connects to GitHub through your registered App
            {slug ? ` (${slug})` : ''}, polling under its own bot identity across {installCount}{' '}
            installation
            {installCount === 1 ? '' : 's'}.
          </p>
          {/* Whether GitHub is actually delivering this App's webhooks here.
              Renders nothing until the backend's probe has an answer — the
              installation mirror is what a hookless App silently costs, and
              this is the only place that says so. */}
          {orgId && (
            <GitHubWebhookHealthNotice
              orgId={orgId}
              health={installStatus?.webhook_health ?? null}
              isLocal={ctx.isLocal}
            />
          )}
          <GitHubInstallationList installations={installations} drift={findings.drift} />
          <GitHubGrantFindings findings={findings} />
          {error && <p className="text-ui text-alarm">{error}</p>}
          {/* The ways off the workspace's own App: the deployment's App where
              the deployment offers one, or a token. Never a bare teardown —
              see switchToDeploymentApp. */}
          <div className="flex flex-wrap items-center gap-2">
            {deploymentAppAvailable && orgId && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void switchToDeploymentApp()}
                className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-40"
              >
                Switch to the deployment&rsquo;s App…
              </button>
            )}
            <button
              type="button"
              disabled={busy}
              onClick={() => setPhase({ kind: 'to-pat-token' })}
              className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-40"
            >
              Switch to a personal access token…
            </button>
          </div>
        </>
      ) : managed ? (
        // The deployment's App. Nothing here registers or imports an App —
        // this workspace rides the operator's — and nothing lists what the
        // shared App can see: the only installations named are the ones this
        // workspace has bound. Zero of them is an ordinary state (a request an
        // owner has yet to approve, an install made from GitHub's side that
        // nobody has connected, an uninstall) rather than a fault, and the way
        // out of it is the same Connect button that binds a first account.
        <>
          <p className="text-body leading-relaxed text-ink-3">
            {installCount === 0 ? (
              <>
                Triage Factory connects to GitHub through this deployment&rsquo;s GitHub App, but no
                GitHub account is connected to this workspace yet. Connect GitHub to install the App
                on an account &mdash; or, if the App is already installed there, to attach that
                installation to this workspace.
              </>
            ) : (
              <>
                Triage Factory connects to GitHub through this deployment&rsquo;s GitHub App,
                installed on {installCount} account{installCount === 1 ? '' : 's'} connected to this
                workspace. Which repositories each account grants is chosen on GitHub&rsquo;s
                installation page, never here.
              </>
            )}
          </p>
          <GitHubInstallationList
            installations={installations}
            drift={findings.drift}
            busy={busy}
            onDisconnect={(inst) => void disconnectManaged(inst)}
          />
          {installCount > 0 && <GitHubGrantFindings findings={findings} />}
          {error && <p className="text-ui text-alarm">{error}</p>}
          {orgId && (
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                disabled={busy}
                onClick={() => void connectManaged()}
                className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-40"
              >
                {installCount === 0 ? 'Connect GitHub…' : 'Connect another account…'}
              </button>
              <ConnectInstalledAccount orgId={orgId} disabled={busy} />
              {/* The way out of the class, for a workspace that wants its own
                  App or a token instead. It releases every bound account; the
                  installations stay on GitHub. */}
              <button
                type="button"
                disabled={busy}
                onClick={() => void disconnectManaged()}
                className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-3 transition-colors hover:border-alarm/40 hover:text-alarm disabled:opacity-40"
              >
                Disconnect from the deployment&rsquo;s App…
              </button>
            </div>
          )}
        </>
      ) : envPat ? (
        // Env-supplied token: reported, not managed. TRIAGE_FACTORY_* wins on
        // read, so a replacement written here would be stored in the keychain
        // and then ignored on every subsequent read — the operator would rotate,
        // see a success, and keep polling with the old token. There's also no
        // honest identity to show: the agents row records the last token bound
        // through a route, which by definition isn't the one in use. So the
        // section states the fact and points at the only control that actually
        // changes anything, which is the shell the process was started from.
        <>
          <p className="text-body leading-relaxed text-ink-3">
            Triage Factory connects to GitHub with the token in{' '}
            <code className="text-ink-2">TRIAGE_FACTORY_GITHUB_BOT_PAT</code>. Environment variables
            take precedence over anything stored here, so this token is managed where the server is
            started — unset that variable to manage GitHub access from Settings.
          </p>
        </>
      ) : s.hasGitHubPat ? (
        <>
          <p className="text-body leading-relaxed text-ink-3">
            Triage Factory connects to GitHub with a personal access token
            {s.githubPatLogin ? (
              <>
                , authenticating as{' '}
                <span className="font-medium text-ink-2">@{s.githubPatLogin}</span>
              </>
            ) : null}
            . Switch to a GitHub App to {appPitch}.
          </p>
          <div className="flex flex-wrap items-center gap-2">
            {/* Rotation lives beside the switch, not inside it: replacing an
                expired or revoked token is routine hygiene, and routing it
                through a mode change (App and back) would be a teardown to swap
                a string. */}
            <button
              type="button"
              onClick={() => setPhase({ kind: 'rotate-token' })}
              className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1"
            >
              Replace token…
            </button>
            <button
              type="button"
              onClick={() => setPhase({ kind: 'to-app-source' })}
              className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1"
            >
              Switch to GitHub App…
            </button>
          </div>
        </>
      ) : (
        // Nothing bound. Every way in is offered at once, none hidden behind
        // another: the deployment's App when the deployment has one (the fast
        // path — a default, never a mandate), an App of the workspace's own,
        // an existing App, or a token.
        <>
          <p className="text-body leading-relaxed text-ink-3">
            GitHub access isn&rsquo;t configured for this workspace yet.{' '}
            {deploymentAppAvailable ? (
              <>
                Connect GitHub to use this deployment&rsquo;s App, or set up a GitHub App of your
                own to {appPitch}. A personal access token works too.
              </>
            ) : (
              <>
                Register a GitHub App to {appPitch}, connect an App that already exists, or use a
                personal access token.
              </>
            )}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            {deploymentAppAvailable && orgId && (
              <>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void connectManaged()}
                  className="rounded-full bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40"
                >
                  Connect GitHub…
                </button>
                <ConnectInstalledAccount orgId={orgId} />
              </>
            )}
            <button
              type="button"
              onClick={() => {
                ctx.patch({ githubAppSource: 'create' })
                setPhase({ kind: 'to-app-account' })
              }}
              className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1"
            >
              Register a GitHub App…
            </button>
            <button
              type="button"
              onClick={() => {
                ctx.patch({ githubAppSource: 'existing' })
                setPhase({ kind: 'to-app-import' })
              }}
              className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1"
            >
              Connect an existing App…
            </button>
            <button
              type="button"
              onClick={() => setPhase({ kind: 'rotate-token' })}
              className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1"
            >
              Use a personal access token…
            </button>
          </div>
        </>
      )}
    </div>
  )
}

// Frame wraps a stepper screen with a Cancel escape that returns to idle. The
// per-screen Actions own the primary button; this only guarantees an exit.
// Cancel is disabled while an operation is in flight: reset() would clear busy
// and snap to idle, but the in-flight async still resolves and calls setPhase
// afterward — yanking the user into a phase (e.g. the diff) they just cancelled.
function Frame({
  children,
  onCancel,
  busy,
  // The rotation screens aren't a switch, so they name their own exit.
  cancelLabel = 'Cancel switch',
}: {
  children: React.ReactNode
  onCancel: () => void
  busy?: boolean
  cancelLabel?: string
}) {
  return (
    <div className="space-y-5">
      {children}
      <button
        type="button"
        onClick={onCancel}
        disabled={busy}
        className="text-ui text-ink-3 underline transition-colors hover:text-ink-2 disabled:opacity-40"
      >
        {cancelLabel}
      </button>
    </div>
  )
}

// StagedBanner is the "registered but not yet active" notice with the two exits.
function StagedBanner({
  slug,
  busy,
  error,
  onFinish,
  onDiscard,
}: {
  slug: string
  busy: boolean
  error: string | null
  onFinish: () => void
  onDiscard: () => void
}) {
  return (
    <div className="space-y-3 rounded-xl border border-warm/20 bg-warm/[0.08] px-4 py-3">
      <p className="text-body leading-relaxed text-warm dark:text-warm">
        GitHub App{slug ? ` (${slug})` : ''} registered but not yet active — finish switching or
        discard. Your personal access token stays the live credential until you switch over.
      </p>
      {error && <p className="text-ui text-alarm">{error}</p>}
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={onFinish}
          disabled={busy}
          className="rounded-full bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40"
        >
          {busy ? 'Working…' : 'Finish switching'}
        </button>
        <button
          type="button"
          onClick={onDiscard}
          disabled={busy}
          className="rounded-xl px-3 py-2 text-body font-medium text-ink-3 transition-colors hover:text-ink-2 disabled:opacity-40"
        >
          Discard
        </button>
      </div>
    </div>
  )
}

// TokenScreen is the token entry shared by the App→PAT switch and the in-place
// rotation: a password field whose Continue runs the pat-preflight (validate +
// diff). The heading/detail are supplied because the two read differently —
// one is changing how the org connects, the other is changing only the string.
function TokenScreen({
  title = 'Switch to a personal access token',
  detail,
  busy,
  error,
  onSubmit,
}: {
  title?: string
  detail?: React.ReactNode
  busy: boolean
  error: string | null
  onSubmit: (pat: string) => void
}) {
  const [pat, setPat] = useState('')
  return (
    <div className="space-y-3">
      <div className="space-y-1.5">
        <h3 className="text-column font-medium text-ink-1">{title}</h3>
        <p className="text-body leading-relaxed text-ink-3">
          {detail ?? (
            <>
              Enter a token with <code className="text-ink-2">repo</code>,{' '}
              <code className="text-ink-2">read:org</code>, and{' '}
              <code className="text-ink-2">user:email</code> scopes. We&rsquo;ll validate it and
              show which repositories it can reach before anything changes.
            </>
          )}
        </p>
      </div>
      <input
        type="password"
        autoComplete="off"
        value={pat}
        placeholder="ghp_…"
        onChange={(e) => setPat(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && pat.trim() !== '') onSubmit(pat.trim())
        }}
        aria-invalid={!!error || undefined}
        className={`w-full rounded-xl border bg-ground px-4 py-2.5 text-body text-ink-1 placeholder-ink-3 transition-colors focus:outline-none focus:ring-2 focus:ring-warm/30 ${
          error ? 'border-alarm/50' : 'border-line-1 focus:border-warm/40'
        }`}
      />
      {error && <p className="text-ui text-alarm">{error}</p>}
      <Actions
        confirmLabel="Continue"
        onConfirm={() => onSubmit(pat.trim())}
        confirmDisabled={pat.trim() === ''}
        busy={busy}
      />
    </div>
  )
}

// AccessDiffScreen renders the inform-only reachability diff with an explicit
// acknowledgment when repos would go dark. Shared by both switch directions and
// the rotation; the optional `login` heads the variants that validated a token
// with the identity it resolved to, and `action` names what the numbers are
// about to describe ("after the switch" / "after the replacement").
function AccessDiffScreen({
  diff,
  login,
  action = 'switch',
  confirmLabel,
  busy,
  error,
  onConfirm,
}: {
  diff: AccessDiff
  login?: string
  action?: string
  confirmLabel: string
  busy: boolean
  error: string | null
  onConfirm: () => void
}) {
  const dark = diff.dark_repos
  const [ack, setAck] = useState(false)
  const needsAck = dark.length > 0
  return (
    <div className="space-y-3">
      {login && (
        <p className="text-body text-ink-2">
          Validated as <span className="font-medium text-ink-1">@{login}</span>.
        </p>
      )}
      <p className="text-body text-ink-2">
        {diff.reachable} of {diff.tracked} tracked{' '}
        {diff.tracked === 1 ? 'repository stays' : 'repositories stay'} reachable after the {action}
        .
      </p>

      {needsAck ? (
        <div className="space-y-2.5">
          <p className="text-body leading-relaxed text-ink-3">
            These repositories will stop updating after the {action}. Existing tasks and open work
            are kept. You can untrack them later in each team&rsquo;s repository settings.
          </p>
          <ul className="space-y-1">
            {dark.map((d) => (
              <li
                key={d.repo}
                className="flex items-center justify-between gap-3 rounded-xl border border-line-1 bg-raised px-3 py-2"
              >
                <span className="text-ui text-ink-1">{d.repo}</span>
                <span className="truncate text-reported text-ink-3" title={d.teams.join(', ')}>
                  {d.teams.length > 0 ? d.teams.join(', ') : 'no team'}
                </span>
              </li>
            ))}
          </ul>
          <label className="flex items-start gap-2 text-ui text-ink-2">
            <input
              type="checkbox"
              checked={ack}
              onChange={(e) => setAck(e.target.checked)}
              className="mt-0.5"
            />
            <span>
              I understand {dark.length} {dark.length === 1 ? 'repository' : 'repositories'} will
              stop updating.
            </span>
          </label>
        </div>
      ) : (
        <p className="text-body leading-relaxed text-ink-3">
          All tracked repositories remain reachable after the {action}.
        </p>
      )}

      {error && <p className="text-ui text-alarm">{error}</p>}
      <Actions
        confirmLabel={confirmLabel}
        onConfirm={onConfirm}
        confirmDisabled={needsAck && !ack}
        busy={busy}
      />
    </div>
  )
}

// Actions is the shared Confirm footer for a stepper screen. Cancel is owned by
// the wrapping Frame ("Cancel switch"), so this footer carries only the primary.
function Actions({
  confirmLabel,
  onConfirm,
  confirmDisabled = false,
  busy,
}: {
  confirmLabel: string
  onConfirm: () => void
  confirmDisabled?: boolean
  busy: boolean
}) {
  return (
    <div className="flex items-center justify-end gap-2">
      <button
        type="button"
        onClick={onConfirm}
        disabled={busy || confirmDisabled}
        className="rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90 disabled:opacity-40 disabled:shadow-none"
      >
        {busy ? 'Working…' : confirmLabel}
      </button>
    </div>
  )
}
