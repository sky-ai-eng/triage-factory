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
          <span className="text-ui font-medium text-ink-1">{label}</span>
          <span className="text-reported text-ink-3 ml-2">{description}</span>
        </div>
        {requireCanonical && (
          <div className="shrink-0 flex items-center gap-1.5">
            <span className="text-label uppercase tracking-wide text-ink-3">
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
              className={`bg-raised border rounded-lg px-2 py-1 text-ui text-ink-1 focus:outline-none focus:ring-2 focus:ring-warm/30 focus:border-warm/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${
                showCanonicalWarning ? 'border-alarm/40' : 'border-line-1'
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
              className={`text-reported px-2.5 py-1 rounded-full border transition-colors ${
                selected
                  ? isCanonical
                    ? 'bg-warm/[0.14] border-warm/50 text-warm font-medium'
                    : 'bg-warm/[0.08] border-warm/25 text-warm font-medium'
                  : 'bg-raised border-line-1 text-ink-3 hover:text-ink-2 hover:border-line-1/80'
              }`}
            >
              {s.name}
            </button>
          )
        })}
      </div>

      {showCanonicalWarning && (
        <div className="text-reported text-alarm">
          Pick a write target — TF needs a specific status to transition into.
        </div>
      )}
    </div>
  )
}
