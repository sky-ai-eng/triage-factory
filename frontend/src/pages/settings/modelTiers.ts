// Shared option sets + type for the model selectors. Kept in a non-component
// module so the component (ModelTierSelector) and its callers (the settings
// sections, the wizard steps) read one definition.
//
// Two different vocabularies live here, and they are not interchangeable. The
// team default and every per-step pin are CATALOG KEYS, built at render time
// from the org's model catalog — see modelOptionsFrom. The workspace cap is a
// rank over the three Anthropic tiers, which is a different kind of value
// (a ceiling, not a choice) and keeps its own words for as long as the column
// does.

import type { ModelCatalogEntry } from '../../hooks/useModelCatalog'

export interface ModelTierOption {
  // The persisted value. A catalog key for a model choice; for the cap, "" (no
  // cap) or one of the three tier words.
  value: string
  label: string
  // One-word hint shown under the label. The cap's rungs carry one; a catalog
  // entry does not, because the catalog asserts no ordering over models and a
  // hint like "most capable" would be exactly that assertion.
  hint?: string
}

// modelOptionsFrom renders a catalog read as picker options: the key is what
// gets stored, the display name is what a person reads. File order is display
// order and carries no ranking.
export function modelOptionsFrom(models: ModelCatalogEntry[]): ModelTierOption[] {
  return models.map((m) => ({ value: m.key, label: m.display_name }))
}

// The workspace cap: the three Anthropic tiers ascending, plus a no-cap end
// ("" — the ceiling lifted). Last in the array because it sits at the top of
// the ladder.
export const MODEL_CAP_OPTIONS: ModelTierOption[] = [
  { value: 'haiku', label: 'Haiku', hint: 'Fastest' },
  { value: 'sonnet', label: 'Sonnet', hint: 'Balanced' },
  { value: 'opus', label: 'Opus', hint: 'Most capable' },
  { value: '', label: 'No cap', hint: 'Uncapped' },
]
