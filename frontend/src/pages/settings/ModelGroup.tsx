import { Section, Field, inputClass } from './primitives'

interface ModelValue {
  max_llm_model_tier: string
}

/**
 * ModelGroup is the org/workspace-scope model field group. Today it carries
 * the max model-tier cap — a hard ceiling over every team's model choice; a
 * team default above the cap is clamped down to it (the team is told, the cap
 * wins). Provider credentials (Anthropic key / Bedrock) belong to this
 * surface too but have no settings UI yet, so they're intentionally omitted
 * until their own ticket lands rather than invented here.
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
        <select
          value={value.max_llm_model_tier}
          onChange={(e) => onChange({ max_llm_model_tier: e.target.value })}
          className={inputClass}
        >
          <option value="">No cap</option>
          <option value="haiku">Haiku</option>
          <option value="sonnet">Sonnet</option>
          <option value="opus">Opus</option>
        </select>
        <p className="text-[11px] text-text-tertiary mt-1">
          Hard ceiling for the whole workspace. A team default above this cap is clamped down to it
          — the team is told, but the cap wins.
        </p>
      </Field>
    </Section>
  )
}
