import { ExternalLink } from 'lucide-react'
import { type GitHubAppInstallation } from '../../lib/githubApp'

/**
 * GitHubAppInstallView is the shared install affordance composed by both the
 * setup wizard's "Install the App" step (GitHubStep) and the Settings "App
 * installation" section (OrgSettings). It owns no gating — the host decides
 * what "installed" means and drives the verify action (the wizard's Continue,
 * Settings' "Check installation"). This is purely the install deep-link plus
 * the list of accounts the App is currently installed on; the host supplies its
 * own surrounding guidance copy. The status + install URL come from the shared
 * useGitHubAppInstall hook, which the host owns so it can also drive the verify.
 */
export function GitHubAppInstallView({
  installations,
  installUrl,
}: {
  installations: GitHubAppInstallation[]
  installUrl: string
}) {
  return (
    <div className="space-y-3">
      {installUrl ? (
        <a
          href={installUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1.5 rounded-xl border border-warm/20 px-4 py-2 text-ui text-warm transition-colors hover:text-warm/80"
        >
          <ExternalLink size={13} />
          Install the App on GitHub
        </a>
      ) : (
        // installUrl is blank only when its fetch failed; without a fallback the
        // view would render nothing actionable (no link, and no accounts yet on
        // first run) — a silent dead-end. Point the user at GitHub directly.
        <p className="text-reported leading-relaxed text-ink-3">
          Couldn&rsquo;t load the install link. Open your GitHub App&rsquo;s page on GitHub to
          install it, then continue.
        </p>
      )}
      {installations.length > 0 && (
        <div className="space-y-1 pt-1">
          <p className="text-reported text-ink-3">Installed on:</p>
          {installations.map((inst) => (
            <div
              key={inst.installation_id}
              className="flex items-center justify-between rounded-xl border border-line-1 bg-raised px-3 py-2"
            >
              <span className="text-ui text-ink-1">{inst.account_login}</span>
              <span className="text-label uppercase tracking-wide text-ink-3">
                {inst.account_type}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
