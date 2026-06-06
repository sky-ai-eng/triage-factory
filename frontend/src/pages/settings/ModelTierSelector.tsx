// ModelTierSelector is the shared, presentational tier picker — a row of
// selectable cards rather than a <select> dropdown. It backs the wizard's org
// "max model tier" step and (per the wizard plan) the team "default model"
// step, so both surfaces render an identical control from one definition. It's
// deliberately option-driven: the org cap offers a "No cap" choice, a team
// default offers the concrete tiers, but the chrome and a11y are the same.
//
// Controlled and value-typed as a plain string so it slots into the existing
// OrgConfigForm.max_llm_model_tier / TeamConfigForm.default_model fields with
// no wire-shape change. radiogroup semantics make it keyboard- and
// screen-reader-navigable as one logical control.

export interface ModelTierOption {
  // The persisted value (e.g. "" for no cap, "haiku" | "sonnet" | "opus").
  value: string
  label: string
  // One-line description shown under the label inside the card.
  hint?: string
}

export default function ModelTierSelector({
  value,
  onChange,
  options,
  ariaLabel,
}: {
  value: string
  onChange: (value: string) => void
  options: ModelTierOption[]
  ariaLabel: string
}) {
  return (
    <div role="radiogroup" aria-label={ariaLabel} className="grid grid-cols-2 gap-2 sm:grid-cols-4">
      {options.map((opt) => {
        const selected = opt.value === value
        return (
          <button
            key={opt.value || 'none'}
            type="button"
            role="radio"
            aria-checked={selected}
            onClick={() => onChange(opt.value)}
            className={`flex flex-col items-start gap-0.5 rounded-xl border px-3.5 py-3 text-left transition-colors ${
              selected
                ? 'border-accent/50 bg-accent/[0.06] shadow-sm shadow-black/[0.03]'
                : 'border-border-subtle bg-white/40 hover:border-accent/30 hover:bg-white/70'
            }`}
          >
            <span
              className={`text-[13px] font-medium ${
                selected ? 'text-accent' : 'text-text-primary'
              }`}
            >
              {opt.label}
            </span>
            {opt.hint && (
              <span className="text-[11px] leading-snug text-text-tertiary">{opt.hint}</span>
            )}
          </button>
        )
      })}
    </div>
  )
}
