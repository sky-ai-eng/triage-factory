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
      {installUrl && (
        <a
          href={installUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1.5 rounded-xl border border-accent/20 px-4 py-2 text-[12px] text-accent transition-colors hover:text-accent/80"
        >
          <ExternalLink size={13} />
          Install the App on GitHub
        </a>
      )}
      {installations.length > 0 && (
        <div className="space-y-1 pt-1">
          <p className="text-[11px] text-text-tertiary">Installed on:</p>
          {installations.map((inst) => (
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
  )
}
