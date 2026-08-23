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

  /** identify re-points a stored ref at the freshly-fetched status it names, so
   *  a member carried over from a rule written before statuses had ids gains
   *  one without being clicked.
   *
   *  This is what makes "editing a rule identifies it" true. Only the status a
   *  click touches arrives with an id of its own; every other member is carried
   *  over verbatim, and the write path can send a rule only when EVERY member
   *  has an id — so one untouched name-only member would have silently held the
   *  whole rule back and reverted the edit on the next load. A ref the list
   *  cannot resolve is left exactly as it is: the board flags it as missing
   *  from the workflow, which is a thing to be told, not to be quietly fixed. */
  const identify = (ref: JiraStatusRef): JiraStatusRef =>
    allStatuses.find((s) => sameStatus(s, ref)) ?? ref

  const emit = (members: JiraStatusRef[], canonical: JiraStatusRef | null | undefined) =>
    onChange({ members: members.map(identify), canonical: canonical && identify(canonical) })

  const toggle = (status: JiraStatusRef) => {
    if (isMember(status)) {
      const nextMembers = value.members.filter((m) => !sameStatus(m, status))
      const nextCanonical =
        value.canonical && sameStatus(value.canonical, status) ? null : value.canonical
      emit(nextMembers, nextCanonical)
    } else {
      const nextMembers = [...value.members, status]
      const nextCanonical =
        requireCanonical && !value.canonical && value.members.length === 0
          ? status
          : value.canonical
      emit(nextMembers, nextCanonical)
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
              value={value.canonical?.id || value.canonical?.name || ''}
              onChange={(e) =>
                emit(
                  value.members,
                  value.members.find((m) => (m.id || m.name) === e.target.value) ?? null,
                )
              }
              disabled={value.members.length === 0}
              className={`bg-raised border rounded-lg px-2 py-1 text-ui text-ink-1 focus:outline-none focus:ring-2 focus:ring-warm/30 focus:border-warm/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${
                showCanonicalWarning ? 'border-alarm/40' : 'border-line-1'
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
