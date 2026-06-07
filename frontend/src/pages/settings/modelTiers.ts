// Shared model-tier option sets + type for the cap / default selectors. Kept in
// a non-component module so the component (ModelTierSelector) and its callers
// (ModelGroup, the wizard steps) read one definition. Order is ascending in
// capability — the ladder selector renders the cap as a ceiling rising along
// these tiers, with the "" (no cap) end lifting it entirely.

export interface ModelTierOption {
  // The persisted value: "" = no cap, else "haiku" | "sonnet" | "opus".
  value: string
  label: string
  // One-word capability hint shown under the label.
  hint?: string
}

// The three concrete tiers, ascending. A team default picks one of these.
export const MODEL_TIER_OPTIONS: ModelTierOption[] = [
  { value: 'haiku', label: 'Haiku', hint: 'Fastest' },
  { value: 'sonnet', label: 'Sonnet', hint: 'Balanced' },
  { value: 'opus', label: 'Opus', hint: 'Most capable' },
]

// The workspace cap: the three tiers plus a no-cap end ("" — the ceiling
// lifted). Last in the array because it sits at the top of the ladder.
export const MODEL_CAP_OPTIONS: ModelTierOption[] = [
  ...MODEL_TIER_OPTIONS,
  { value: '', label: 'No cap', hint: 'Uncapped' },
]
