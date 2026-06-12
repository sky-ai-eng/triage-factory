// GitHubAccessControl is the body of Settings' single "GitHub access" section —
// the post-TFAC-328 replacement for the old independently-editable App/PAT
// credential sections. GitHub access is either/or per org, so this surface shows
// the LIVE mode plainly and offers exactly one affordance: an explicit, guided
// switch to the other mode (with an inform-only reachability diff and a confirm
// step), plus the staged-switch banner when a PAT→App switch is registered but
// not yet cut over.
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

import { useState } from 'react'
import { ExternalLink } from 'lucide-react'
import { toast } from '../../../components/Toast/toastStore'
import { isHttpUrl } from '../../../lib/reachability'
import { GitHubAccountTypeStep, GitHubAppStep } from '../../setup/GitHubStep'
import { GitHubAppInstallView } from '../GitHubAppInstallView'
import { useGitHubAppInstall } from '../../../hooks/useGitHubAppInstall'
import {
  cutoverPreflight,
  cutoverToApp,
  discardStagedApp,
  patPreflight,
  refreshGitHubAppInstallations,
  switchToPat,
  type AccessDiff,
} from '../../../lib/githubApp'
import type { StepContext } from '../../setup/types'

// The guided-switch state machine. `idle` shows the live mode + the switch
// affordance (or the staged banner); the rest are the stepper screens, split by
// direction. The register screen is transient — clicking Register redirects
// away, and the user returns to `idle` on the staged banner.
type Phase =
  | { kind: 'idle' }
  | { kind: 'to-app-account' }
  | { kind: 'to-app-register' }
  | { kind: 'to-app-install' }
  | { kind: 'to-app-diff'; diff: AccessDiff }
  | { kind: 'to-pat-token' }
  | { kind: 'to-pat-diff'; diff: AccessDiff; login: string; pat: string }
  | { kind: 'to-pat-success'; settingsUrl: string }

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

  const [phase, setPhase] = useState<Phase>({ kind: 'idle' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Install state for the to-app-install screen (and to read the live
  // installation count for the staged banner's resume target). Refetches on
  // focus, so returning from the GitHub install tab updates the list.
  const { status: installStatus, installUrl } = useGitHubAppInstall(orgId)
  const installCount = installStatus?.installations.length ?? s.githubAppInstallCount

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

  // ── App→PAT: commit the teardown ──
  const commitSwitchToPat = async (pat: string) => {
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
      setPhase({ kind: 'to-pat-success', settingsUrl })
      setBusy(false)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  // ─────────────────────────────── render ───────────────────────────────

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
        <p className="text-[13px] leading-relaxed text-text-tertiary">
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
          <h3 className="text-[15px] font-medium text-text-primary">Install the App</h3>
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            GitHub only grants repository access once the App is installed. Install it on your
            account or organization, then continue to review the switch.
          </p>
        </div>
        <GitHubAppInstallView
          installations={installStatus?.installations ?? []}
          installUrl={installUrl}
        />
        {error && <p className="text-[12px] text-dismiss">{error}</p>}
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
          onConfirm={() => void commitSwitchToPat(phase.pat)}
        />
      </Frame>
    )
  }

  // to-pat-success — the App is torn down locally; guide deleting it on GitHub
  // and flag the identity-capture downgrade.
  if (phase.kind === 'to-pat-success') {
    return (
      <div className="space-y-4">
        <div className="rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-3">
          <p className="text-[13px] font-medium text-claim">Switched to a personal access token</p>
        </div>
        <p className="text-[13px] leading-relaxed text-text-secondary">
          The GitHub App still exists on GitHub — delete it there if you no longer need it:
          {phase.settingsUrl ? (
            <>
              {' '}
              <a
                href={phase.settingsUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-accent hover:underline"
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
        <p className="text-[13px] leading-relaxed text-text-tertiary">
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
              })
              reset()
              reload()
            }}
            className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white transition-colors hover:bg-accent/90"
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
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            Triage Factory connects to GitHub through your registered App
            {slug ? ` (${slug})` : ''}, polling under its own bot identity across {installCount}{' '}
            installation
            {installCount === 1 ? '' : 's'}.
          </p>
          <button
            type="button"
            onClick={() => setPhase({ kind: 'to-pat-token' })}
            className="rounded-xl border border-border-glass px-4 py-2 text-[13px] font-medium text-text-secondary transition-colors hover:border-accent/40 hover:text-text-primary"
          >
            Switch to a personal access token…
          </button>
        </>
      ) : (
        <>
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            {s.hasGitHubPat
              ? 'Triage Factory connects to GitHub with a personal access token. Switch to a GitHub App to poll under its own bot identity with support for multiple installations.'
              : 'GitHub access isn’t configured for this workspace yet. Register a GitHub App to poll under its own bot identity with support for multiple installations.'}
          </p>
          <button
            type="button"
            onClick={() => setPhase({ kind: 'to-app-account' })}
            className="rounded-xl border border-border-glass px-4 py-2 text-[13px] font-medium text-text-secondary transition-colors hover:border-accent/40 hover:text-text-primary"
          >
            {s.hasGitHubPat ? 'Switch to GitHub App…' : 'Set up a GitHub App…'}
          </button>
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
}: {
  children: React.ReactNode
  onCancel: () => void
  busy?: boolean
}) {
  return (
    <div className="space-y-5">
      {children}
      <button
        type="button"
        onClick={onCancel}
        disabled={busy}
        className="text-[12px] text-text-tertiary underline transition-colors hover:text-text-secondary disabled:opacity-40"
      >
        Cancel switch
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
    <div className="space-y-3 rounded-xl border border-amber-500/20 bg-amber-500/[0.08] px-4 py-3">
      <p className="text-[13px] leading-relaxed text-amber-600 dark:text-amber-400">
        GitHub App{slug ? ` (${slug})` : ''} registered but not yet active — finish switching or
        discard. Your personal access token stays the live credential until you switch over.
      </p>
      {error && <p className="text-[12px] text-dismiss">{error}</p>}
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={onFinish}
          disabled={busy}
          className="rounded-full bg-accent px-5 py-2 text-[13px] font-medium text-white transition-colors hover:bg-accent/90 disabled:opacity-40"
        >
          {busy ? 'Working…' : 'Finish switching'}
        </button>
        <button
          type="button"
          onClick={onDiscard}
          disabled={busy}
          className="rounded-xl px-3 py-2 text-[13px] font-medium text-text-tertiary transition-colors hover:text-text-secondary disabled:opacity-40"
        >
          Discard
        </button>
      </div>
    </div>
  )
}

// TokenScreen is the App→PAT token entry: a password field whose Continue runs
// the pat-preflight (validate + diff).
function TokenScreen({
  busy,
  error,
  onSubmit,
}: {
  busy: boolean
  error: string | null
  onSubmit: (pat: string) => void
}) {
  const [pat, setPat] = useState('')
  return (
    <div className="space-y-3">
      <div className="space-y-1.5">
        <h3 className="text-[15px] font-medium text-text-primary">
          Switch to a personal access token
        </h3>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Enter a token with <code className="text-text-secondary">repo</code> and{' '}
          <code className="text-text-secondary">read:org</code> scopes. We&rsquo;ll validate it and
          show which repositories it can reach before anything changes.
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
        className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary transition-colors focus:outline-none focus:ring-2 focus:ring-accent/30 ${
          error ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
        }`}
      />
      {error && <p className="text-[12px] text-dismiss">{error}</p>}
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
// acknowledgment when repos would go dark. Shared by both directions; the
// optional `login` heads the App→PAT variant with the validated identity.
function AccessDiffScreen({
  diff,
  login,
  confirmLabel,
  busy,
  error,
  onConfirm,
}: {
  diff: AccessDiff
  login?: string
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
        <p className="text-[13px] text-text-secondary">
          Validated as <span className="font-medium text-text-primary">@{login}</span>.
        </p>
      )}
      <p className="text-[13px] text-text-secondary">
        {diff.reachable} of {diff.tracked} tracked{' '}
        {diff.tracked === 1 ? 'repository stays' : 'repositories stay'} reachable after the switch.
      </p>

      {needsAck ? (
        <div className="space-y-2.5">
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            These repositories will stop updating after the switch. Existing tasks and open work are
            kept. You can untrack them later in each team&rsquo;s repository settings.
          </p>
          <ul className="space-y-1">
            {dark.map((d) => (
              <li
                key={d.repo}
                className="flex items-center justify-between gap-3 rounded-xl border border-border-subtle bg-white/40 px-3 py-2"
              >
                <span className="text-[12px] text-text-primary">{d.repo}</span>
                <span
                  className="truncate text-[11px] text-text-tertiary"
                  title={d.teams.join(', ')}
                >
                  {d.teams.length > 0 ? d.teams.join(', ') : 'no team'}
                </span>
              </li>
            ))}
          </ul>
          <label className="flex items-start gap-2 text-[12px] text-text-secondary">
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
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          All tracked repositories remain reachable after the switch.
        </p>
      )}

      {error && <p className="text-[12px] text-dismiss">{error}</p>}
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
        className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white shadow-[0_10px_28px_-10px_var(--color-accent)] transition-all hover:bg-accent/90 disabled:opacity-40 disabled:shadow-none"
      >
        {busy ? 'Working…' : confirmLabel}
      </button>
    </div>
  )
}
