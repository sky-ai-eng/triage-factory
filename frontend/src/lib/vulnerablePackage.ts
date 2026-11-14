import type { DependabotSeverity, VulnerablePackage } from '../types'
import type { EventTone } from './eventDisplay'

// Display helpers for a vulnerable-package task: the package's facts as the
// board card, the triage deck and the run page state them. Every value comes
// from the task read's typed `vulnerable_package` field; nothing here parses
// a key or a title.

const SEVERITY_LABEL: Record<DependabotSeverity, string> = {
  low: 'Low',
  medium: 'Medium',
  high: 'High',
  critical: 'Critical',
}

/** A severity as a word, or the raw value for one this build does not know. */
export function severityLabel(severity: string): string {
  return SEVERITY_LABEL[severity as DependabotSeverity] ?? severity
}

/** High and critical spend the problem hue, medium the attention one; low is
 *  ink. The same four tones the board's event labels draw from. */
export function severityTone(severity: string | undefined): EventTone {
  if (severity === 'critical' || severity === 'high') return 'problem'
  if (severity === 'medium') return 'attention'
  return 'neutral'
}

/** "1 open alert", "3 open alerts", "No open alerts". */
export function openAlertsLabel(count: number): string {
  if (count === 0) return 'No open alerts'
  return `${count} open alert${count === 1 ? '' : 's'}`
}

/** An alert's dependency scope as a word. */
export function scopeLabel(scope: string): string {
  if (scope === 'runtime') return 'Runtime'
  if (scope === 'development') return 'Development'
  return scope
}

/** The facts a card states beside a vulnerable package's title: its
 *  repository, highest open severity and open alert count. The title already
 *  names the package and its ecosystem. */
export function packageFacts(
  pkg: VulnerablePackage,
): { text: string; tone?: EventTone; title?: string }[] {
  const facts: { text: string; tone?: EventTone; title?: string }[] = [
    { text: pkg.repo, title: `${pkg.package} (${pkg.ecosystem}) in ${pkg.repo}` },
  ]
  if (pkg.highest_severity) {
    facts.push({
      text: severityLabel(pkg.highest_severity),
      tone: severityTone(pkg.highest_severity),
      title: 'Highest severity among the open alerts',
    })
  }
  facts.push({ text: openAlertsLabel(pkg.open_alert_count) })
  return facts
}
