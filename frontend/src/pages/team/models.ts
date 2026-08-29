import type { ModelCatalogEntry } from '../../lib/models'
import type { TeamModelConfig } from '../../hooks/useTeamModelConfig'
import type { TeamUsage } from '../../hooks/useTeamUsage'

// The model surface's shared readings — what the band cell and the opened
// panel both derive their rows from. Separate from the panel component because
// the band lives in TeamSettings, and a component file that also exports
// functions is what react-refresh objects to.

/** Dollars, trimmed the way a rate card reads: cents only when they carry
 * information. Null — a row whose cost the harness settles — is a dash. */
export function money(n: number | null): string {
  if (n == null) return '—'
  return '$' + (n < 1 ? n.toFixed(2) : n % 1 ? n.toFixed(2) : String(n))
}

/** The team's effective enable-set, in the catalog's own order: the stored set
 * when the team has one (narrowed to what the org still enables — the org may
 * shrink its set and nothing rewrites team rows when it does), else the org's
 * whole set. */
export function effectiveModelKeys(
  models: ModelCatalogEntry[],
  config: TeamModelConfig | null,
): string[] {
  if (!config) return []
  if (config.enabledModels == null) return models.map((m) => m.key)
  const stored = new Set(config.enabledModels)
  return models.filter((m) => stored.has(m.key)).map((m) => m.key)
}

/** Each model's share of the window's model-attributed spend, as a fraction.
 * Null while the usage read has not answered, and unknown is not zero. The cut
 * itself names models rather than people, so every member receives it. A model
 * absent from the cut genuinely spent nothing, so a miss on this map reads as 0
 * only for a team-enabled row. */
export function spendShares(usage: TeamUsage | null): Map<string, number> | null {
  if (!usage) return null
  const buckets = usage.by_model ?? []
  const total = buckets.reduce((n, b) => n + b.cost, 0)
  return new Map(buckets.map((b) => [b.model, total ? b.cost / total : 0]))
}
