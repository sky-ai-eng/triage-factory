import { Section, Field } from './primitives'
import ModelTierSelector, { type ModelTierOption } from './ModelTierSelector'

interface ModelValue {
  max_llm_model_tier: string
}

// The workspace-cap options: an explicit "No cap" plus the three tiers. The
// hints frame the cap as a ceiling. Shared by Settings and the wizard's org
// max-tier step, so the choice reads identically in both.
const CAP_OPTIONS: ModelTierOption[] = [
  { value: '', label: 'No cap', hint: 'Teams choose freely' },
  { value: 'haiku', label: 'Haiku', hint: 'Fastest, cheapest' },
  { value: 'sonnet', label: 'Sonnet', hint: 'Balanced' },
  { value: 'opus', label: 'Opus', hint: 'Most capable' },
]

/**
 * ModelGroup is the org/workspace-scope model field group. Today it carries
 * the max model-tier cap — a hard ceiling over every team's model choice; a
 * team default above the cap is clamped down to it (the team is told, the cap
 * wins). It renders the shared ModelTierSelector (a card row, not a dropdown)
 * so Settings and the setup wizard present one identical control. Provider
 * credentials (Anthropic key / Bedrock) belong to this surface too but have no
 * settings UI yet, so they're intentionally omitted until their own ticket
 * lands rather than invented here.
 */
export default function ModelGroup({
  value,
  onChange,
}: {
  value: ModelValue
  onChange: (patch: Partial<ModelValue>) => void
}) {
  return (
    <Section>
      <h2 className="text-[13px] font-medium text-text-secondary mb-4">AI</h2>
      <Field label="Max model tier (workspace cap)">
        <ModelTierSelector
          value={value.max_llm_model_tier}
          onChange={(max_llm_model_tier) => onChange({ max_llm_model_tier })}
          options={CAP_OPTIONS}
          ariaLabel="Maximum model tier (workspace cap)"
        />
        <p className="text-[11px] text-text-tertiary mt-2">
          Hard ceiling for the whole workspace. A team default above this cap is clamped down to it
          — the team is told, but the cap wins.
        </p>
      </Field>
    </Section>
  )
}
