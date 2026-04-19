interface JiraStatus {
  id: string
  name: string
}

export interface JiraStatusRuleValue {
  members: string[]
  canonical?: string
}

interface Props {
  label: string
  description: string
  allStatuses: JiraStatus[]
  value: JiraStatusRuleValue
  onChange: (next: JiraStatusRuleValue) => void
  requireCanonical: boolean
  canonicalPrompt?: string
}

export default function JiraStatusRule({
  label,
  description,
  allStatuses,
  value,
  onChange,
  requireCanonical,
  canonicalPrompt,
}: Props) {
  const toggle = (name: string) => {
    if (value.members.includes(name)) {
      const nextMembers = value.members.filter((n) => n !== name)
      const nextCanonical = value.canonical === name ? undefined : value.canonical
      onChange({ members: nextMembers, canonical: nextCanonical })
    } else {
      const nextMembers = [...value.members, name]
      const nextCanonical =
        requireCanonical && !value.canonical && value.members.length === 0 ? name : value.canonical
      onChange({ members: nextMembers, canonical: nextCanonical })
    }
  }

  const showCanonicalWarning = requireCanonical && value.members.length > 0 && !value.canonical

  return (
    <div className="space-y-2">
      <div className="flex items-baseline justify-between gap-3">
        <div className="min-w-0 leading-tight">
          <span className="text-[12px] font-medium text-text-primary">{label}</span>
          <span className="text-[11px] text-text-tertiary ml-2">{description}</span>
        </div>
        {requireCanonical && (
          <div className="shrink-0 flex items-center gap-1.5">
            <span className="text-[10px] uppercase tracking-wide text-text-tertiary">
              {canonicalPrompt || 'Write to'}
            </span>
            <select
              value={value.canonical || ''}
              onChange={(e) =>
                onChange({
                  members: value.members,
                  canonical: e.target.value || undefined,
                })
              }
              disabled={value.members.length === 0}
              className={`bg-white/50 border rounded-lg px-2 py-1 text-[12px] text-text-primary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${
                showCanonicalWarning ? 'border-dismiss/40' : 'border-border-subtle'
              }`}
            >
              <option value="">{value.members.length === 0 ? 'pick below' : 'choose…'}</option>
              {value.members.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </div>
        )}
      </div>

      <div className="flex flex-wrap gap-1.5">
        {allStatuses.map((s) => {
          const selected = value.members.includes(s.name)
          const isCanonical = requireCanonical && value.canonical === s.name
          return (
            <button
              key={s.id}
              type="button"
              onClick={() => toggle(s.name)}
              className={`text-[11px] px-2.5 py-1 rounded-full border transition-colors ${
                selected
                  ? isCanonical
                    ? 'bg-accent/[0.14] border-accent/50 text-accent font-medium'
                    : 'bg-accent/[0.08] border-accent/25 text-accent font-medium'
                  : 'bg-white/50 border-border-subtle text-text-tertiary hover:text-text-secondary hover:border-border-subtle/80'
              }`}
            >
              {s.name}
            </button>
          )
        })}
      </div>

      {showCanonicalWarning && (
        <div className="text-[11px] text-dismiss">
          Pick a write target — TF needs a specific status to transition into.
        </div>
      )}
    </div>
  )
}
