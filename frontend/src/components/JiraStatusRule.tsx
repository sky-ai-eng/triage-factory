/** JiraStatusRef is one workflow status as the API speaks it: the id the rules
 *  are keyed on, and the display name the SERVER resolved for that id.
 *
 *  The id is the identity. A Jira workflow references the status entity, so an
 *  id survives a rename and a name does not — which is why the write path sends
 *  ids and never names, and why a name that appears here is always one Jira
 *  gave us, as of the rule's last save. A ref carrying a name and no id is a
 *  rule stored before statuses were identified; it renders and polls fine, and
 *  gains its id when that rule is next saved. */
export interface JiraStatusRef {
  id: string
  name: string
}

export interface JiraStatusRuleValue {
  members: JiraStatusRef[]
  /** The status TF transitions a ticket INTO. Always one of `members`. Null on
   *  a rule nobody has mapped yet, which is a valid saved state. */
  canonical?: JiraStatusRef | null
}

interface Props {
  label: string
  description: string
  allStatuses: JiraStatusRef[]
  value: JiraStatusRuleValue
  onChange: (next: JiraStatusRuleValue) => void
  requireCanonical: boolean
  canonicalPrompt?: string
}

/** sameStatus compares two refs the way the server does: ids decide when both
 *  carry one, names otherwise — which is what lets a legacy name-only member
 *  still match the status it names in the freshly-fetched list. */
const sameStatus = (a: JiraStatusRef, b: JiraStatusRef): boolean =>
  a.id && b.id ? a.id === b.id : !!a.name && a.name === b.name

export default function JiraStatusRule({
  label,
  description,
  allStatuses,
  value,
  onChange,
  requireCanonical,
  canonicalPrompt,
}: Props) {
  const isMember = (status: JiraStatusRef) => value.members.some((m) => sameStatus(m, status))

  const toggle = (status: JiraStatusRef) => {
    if (isMember(status)) {
      const nextMembers = value.members.filter((m) => !sameStatus(m, status))
      const nextCanonical =
        value.canonical && sameStatus(value.canonical, status) ? null : value.canonical
      onChange({ members: nextMembers, canonical: nextCanonical })
    } else {
      const nextMembers = [...value.members, status]
      const nextCanonical =
        requireCanonical && !value.canonical && value.members.length === 0
          ? status
          : value.canonical
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
              value={value.canonical?.id || value.canonical?.name || ''}
              onChange={(e) =>
                onChange({
                  members: value.members,
                  canonical: value.members.find((m) => (m.id || m.name) === e.target.value) ?? null,
                })
              }
              disabled={value.members.length === 0}
              className={`bg-white/50 border rounded-lg px-2 py-1 text-[12px] text-text-primary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${
                showCanonicalWarning ? 'border-dismiss/40' : 'border-border-subtle'
              }`}
            >
              <option value="">{value.members.length === 0 ? 'pick below' : 'choose…'}</option>
              {value.members.map((m) => (
                <option key={m.id || m.name} value={m.id || m.name}>
                  {m.name}
                </option>
              ))}
            </select>
          </div>
        )}
      </div>

      <div className="flex flex-wrap gap-1.5">
        {allStatuses.map((s) => {
          const selected = isMember(s)
          const isCanonical =
            requireCanonical && !!value.canonical && sameStatus(value.canonical, s)
          return (
            <button
              key={s.id || s.name}
              type="button"
              onClick={() => toggle(s)}
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
