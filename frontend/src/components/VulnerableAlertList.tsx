import { ExternalLink } from 'lucide-react'
import type { DependabotAlert } from '../types'
import { scopeLabel, severityLabel, severityTone } from '../lib/vulnerablePackage'
import { TONE_TEXT } from './board/cardStyle'

// VulnerableAlertList — the open Dependabot alerts on a vulnerable package,
// in the order the task read gives them (highest severity first). Each names
// its advisory, severity, manifest, dependency scope and first patched
// version, and links to the alert on GitHub.
//
// The advisory summary is written outside the org, so it renders as text: no
// markup, no linkified URLs. React escapes it; nothing here may change that.

export default function VulnerableAlertList({
  alerts,
  className = '',
}: {
  alerts: DependabotAlert[]
  className?: string
}) {
  if (alerts.length === 0) {
    return <p className={`text-reported text-ink-3 ${className}`}>No open alerts.</p>
  }
  return (
    <ul className={`space-y-2.5 ${className}`} aria-label="Open Dependabot alerts">
      {alerts.map((a) => (
        <li key={a.number} className="min-w-0">
          <div className="flex min-w-0 items-baseline gap-2 font-mono text-reported">
            <span
              className={`shrink-0 font-semibold uppercase tracking-[0.08em] ${TONE_TEXT[severityTone(a.severity)]}`}
            >
              {severityLabel(a.severity)}
            </span>
            <a
              href={a.url}
              target="_blank"
              rel="noopener noreferrer"
              title={`Open alert #${a.number} on GitHub`}
              className="inline-flex min-w-0 items-center gap-1 text-ink-2 transition-colors hover:text-warm"
              onClick={(e) => e.stopPropagation()}
              onPointerDown={(e) => e.stopPropagation()}
            >
              <span className="truncate">{a.ghsa_id || `Alert #${a.number}`}</span>
              <ExternalLink size={10} className="shrink-0" aria-hidden />
            </a>
            {a.cve_id && <span className="truncate text-ink-3">{a.cve_id}</span>}
          </div>
          {a.summary && (
            <p className="mt-0.5 text-secondary leading-snug text-ink-2 break-words">{a.summary}</p>
          )}
          <div className="mt-0.5 flex flex-wrap gap-x-2 font-mono text-label text-ink-3">
            <span className="break-all">{a.manifest_path}</span>
            {a.scope && <span>{scopeLabel(a.scope)}</span>}
            <span>
              {a.first_patched_version
                ? `Fixed in ${a.first_patched_version}`
                : 'No patched version'}
            </span>
          </div>
        </li>
      ))}
    </ul>
  )
}
