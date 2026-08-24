// Shared option shape + builder for the model selectors. Kept in a
// non-component module so the component (ModelTierSelector) and its callers
// (the settings sections, the wizard steps) read one definition.
//
// One vocabulary: every stored model value — a team default, a per-step pin,
// the background-jobs knob — is a CATALOG KEY, built at render time from the
// org's model catalog. See modelOptionsFrom.

import type { ModelCatalogEntry } from '../../hooks/useModelCatalog'

export interface ModelTierOption {
  // The persisted value: a catalog key.
  value: string
  label: string
  // One-word hint shown under the label. A catalog entry carries none, because
  // the catalog asserts no ordering over models and a hint like "most capable"
  // would be exactly that assertion.
  hint?: string
}

// modelOptionsFrom renders a catalog read as picker options: the key is what
// gets stored, the display name is what a person reads. File order is display
// order and carries no ranking.
export function modelOptionsFrom(models: ModelCatalogEntry[]): ModelTierOption[] {
  return models.map((m) => ({ value: m.key, label: m.display_name }))
}
