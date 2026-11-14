import type { VulnerablePackage } from '../types'
import { packageFacts } from '../lib/vulnerablePackage'
import { TONE_TEXT } from './board/cardStyle'

// PackageFacts — a vulnerable package's repository, highest open severity and
// open alert count as one mono readout, separated by middle dots, for the
// surfaces that show one task large: the triage deck and the run page. The
// board card states the same facts through TaskCard's own readout line.
export default function PackageFacts({
  pkg,
  className = '',
}: {
  pkg: VulnerablePackage
  className?: string
}) {
  return (
    <p className={`font-mono text-reported text-ink-3 ${className}`}>
      {packageFacts(pkg).map((f, i) => (
        <span key={i}>
          {i > 0 && (
            <span aria-hidden className="px-1.5 text-ink-4">
              ·
            </span>
          )}
          <span title={f.title} className={f.tone ? TONE_TEXT[f.tone] : undefined}>
            {f.text}
          </span>
        </span>
      ))}
    </p>
  )
}
