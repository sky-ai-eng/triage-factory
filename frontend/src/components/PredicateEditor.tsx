import { useEffect, useState } from 'react'
import { Tooltip } from '../ui/tooltip/Tooltip'
import { Info, X } from 'lucide-react'
import type { FieldSchema } from '../types'
import IdentityListField, { type IdentityKind } from './IdentityListField'
import SlackChannelListField from './SlackChannelListField'
import RepoListField from './RepoListField'
import LinearTeamListField from './LinearTeamListField'
import JiraProjectListField from './JiraProjectListField'
import CatalogListField from './CatalogListField'
import { catalogKindFor } from './predicateCatalog'
import GitHubTeamListField from './GitHubTeamListField'
import { apiJSON, httpErrorMessage } from '../lib/apiClient'

interface PredicateEditorProps {
  eventType: string
  /** The handler's team, whose tracked Slack channels, repositories, Linear
   *  teams and Jira projects `channel_in`, `repo_in`, `linear_team_id_in` and
   *  `jira_project_id_in` offer, whose GitHub-team mappings
   *  `requested_team_id_in` offers, and whose tracked Jira projects and Linear
   *  teams the status, priority, issue type and label fields offer values of.
   *  '' when the server resolves the team (the sole team). */
  teamId?: string
  value: Record<string, unknown>
  onChange: (value: Record<string, unknown>) => void
}

export default function PredicateEditor({
  eventType,
  teamId = '',
  value,
  onChange,
}: PredicateEditorProps) {
  const [fields, setFields] = useState<FieldSchema[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  // Standard "fetch on prop change" pattern: kick off a network call and
  // synchronously flip loading/error to reflect the in-flight state. The
  // sync setState before fetch costs one cascading render per eventType
  // change — fine for a config panel — and the clean alternatives
  // (Suspense, data-fetching library) would be a much bigger refactor.
  // The render guards on !eventType before reading `fields`, so we don't
  // need to clear `fields` explicitly when eventType empties out.
  useEffect(() => {
    if (!eventType) return
    let cancelled = false
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLoading(true)
    setError('')

    apiJSON<{ fields?: FieldSchema[] }>(`/api/event-schemas/${encodeURIComponent(eventType)}`)
      .then((data) => {
        if (!cancelled) {
          setFields(data.fields || [])
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(httpErrorMessage(err, `Could not load the schema for ${eventType}.`))
          setFields([])
          setLoading(false)
        }
      })

    return () => {
      cancelled = true
    }
  }, [eventType])

  const setField = (name: string, val: unknown) => {
    const next = { ...value }
    if (val === undefined || val === null || val === '') {
      delete next[name]
    } else {
      next[name] = val
    }
    onChange(next)
  }

  if (!eventType) {
    return <p className="text-ui text-ink-3 italic">Select an event type first.</p>
  }

  if (loading) {
    return (
      <div className="space-y-3">
        {[1, 2, 3].map((i) => (
          <div key={i} className="h-10 rounded-lg bg-tint-2 animate-pulse" />
        ))}
      </div>
    )
  }

  if (error) {
    return <p className="text-ui text-alarm">{error}</p>
  }

  if (fields.length === 0) {
    return <p className="text-ui text-ink-3 italic">No filterable fields for this event type.</p>
  }

  return (
    <div className="space-y-3">
      {fields.map((field) => (
        <FieldRow
          key={field.name}
          field={field}
          eventType={eventType}
          teamId={teamId}
          value={value[field.name]}
          onChange={(val) => setField(field.name, val)}
        />
      ))}
    </div>
  )
}

// --- Per-field rendering ---------------------------------------------------

interface FieldRowProps {
  field: FieldSchema
  eventType: string
  teamId: string
  value: unknown
  onChange: (val: unknown) => void
}

function FieldRow({ field, eventType, teamId, value, onChange }: FieldRowProps) {
  // A Jira status, priority or issue type, or a Linear state, label or label
  // group, named by id and picked from what the team's tracked sources offer.
  const catalog = field.type === 'string_list' ? catalogKindFor(eventType, field.name) : null
  // A list over a fixed vocabulary the schema names, as ecosystem_in is.
  const enumList =
    field.type === 'string_list' && !!field.enum_values && field.enum_values.length > 0
  // A list of free text, as package_in is: names TF holds no catalog of.
  const textList = field.type === 'string_list' && TEXT_LIST_FIELDS.has(field.name)
  return (
    <div>
      <div className="flex items-center gap-1.5 mb-1.5">
        <label className="text-ui font-medium text-ink-2">{humanize(field.name)}</label>
        {field.description && (
          <Tooltip content={field.description} wrap>
            <Info size={12} className="text-ink-3 cursor-help" />
          </Tooltip>
        )}
      </div>

      {field.type === 'bool' && (
        <BoolField value={value as boolean | undefined} onChange={onChange} />
      )}
      {field.type === 'string' && field.enum_values && field.enum_values.length > 0 && (
        <EnumField
          value={value as string | undefined}
          options={field.enum_values}
          onChange={onChange}
        />
      )}
      {field.type === 'string' && (!field.enum_values || field.enum_values.length === 0) && (
        <StringField value={value as string | undefined} onChange={onChange} />
      )}
      {field.type === 'int' && <IntField value={value as number | undefined} onChange={onChange} />}
      {enumList && (
        <EnumListField
          label={humanize(field.name)}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          options={field.enum_values!}
          onChange={onChange}
        />
      )}
      {textList && (
        <TextListField
          label={humanize(field.name)}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={onChange}
        />
      )}
      {field.type === 'string_list' && field.name === 'channel_in' && (
        <SlackChannelListField
          teamId={teamId}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={(v) => onChange(v)}
        />
      )}
      {field.type === 'string_list' && field.name === 'repo_in' && (
        <RepoListField
          teamId={teamId}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={(v) => onChange(v)}
        />
      )}
      {field.type === 'string_list' && LINEAR_TEAM_FIELDS.has(field.name) && (
        <LinearTeamListField
          teamId={teamId}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={(v) => onChange(v)}
        />
      )}
      {field.type === 'string_list' && JIRA_PROJECT_FIELDS.has(field.name) && (
        <JiraProjectListField
          teamId={teamId}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={(v) => onChange(v)}
        />
      )}
      {catalog && (
        <CatalogListField
          kind={catalog}
          teamId={teamId}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={(v) => onChange(v)}
        />
      )}
      {field.type === 'string_list' && field.name === 'requested_team_id_in' && (
        <GitHubTeamListField
          teamId={teamId}
          value={Array.isArray(value) ? (value as string[]) : undefined}
          onChange={(v) => onChange(v)}
        />
      )}
      {/* Every other string_list field is an identity allowlist:
          GitHub → author_id_in / reviewer_id_in / commenter_id_in /
          requested_user_id_in;
          Jira → assignee_in / reporter_in / commenter_in;
          Linear → assignee_in / creator_in / commenter_in.
          IdentityListField branches on identityKind to read the right
          column (github_user_id, jira_account_id or linear_user_id)
          from /api/me and the team roster. */}
      {field.type === 'string_list' &&
        !REFERENCE_LIST_FIELDS.has(field.name) &&
        !catalog &&
        !enumList &&
        !textList && (
          <IdentityListField
            fieldName={field.name}
            identityKind={identityKindFor(eventType)}
            value={Array.isArray(value) ? (value as string[]) : undefined}
            onChange={(v) => onChange(v)}
          />
        )}
    </div>
  )
}

// LINEAR_TEAM_FIELDS are the Linear team filters: linear_team_id_in on every
// Linear event, and identifier_changed's old_linear_team_id_in.
const LINEAR_TEAM_FIELDS = new Set(['linear_team_id_in', 'old_linear_team_id_in'])

// JIRA_PROJECT_FIELDS are the Jira project filters: jira_project_id_in on
// every Jira event, and key_changed's old_jira_project_id_in.
const JIRA_PROJECT_FIELDS = new Set(['jira_project_id_in', 'old_jira_project_id_in'])

// REFERENCE_LIST_FIELDS are the string_list fields that name rows the team
// tracks or maps, each with its own picker, rather than people.
const REFERENCE_LIST_FIELDS = new Set([
  'channel_in',
  'repo_in',
  'requested_team_id_in',
  ...LINEAR_TEAM_FIELDS,
  ...JIRA_PROJECT_FIELDS,
])

// TEXT_LIST_FIELDS are the string_list fields whose values are free text: a
// package name, in its ecosystem's own spelling. The route compares them
// ignoring case and refuses a repeat, so the field drops one too.
const TEXT_LIST_FIELDS = new Set(['package_in'])

// identityKindFor picks the identity an allowlist holds from the event's
// source prefix: a Linear assignee is a Linear user id, never a GitHub
// account id.
function identityKindFor(eventType: string): IdentityKind {
  if (eventType.startsWith('jira:')) return 'jira'
  if (eventType.startsWith('linear:')) return 'linear'
  return 'github'
}

// --- Bool: tri-state pills [Any] [Yes] [No] --------------------------------

function BoolField({
  value,
  onChange,
}: {
  value: boolean | undefined
  onChange: (v: unknown) => void
}) {
  const isUnset = value === undefined || value === null
  return (
    <div className="flex gap-1">
      <Pill active={isUnset} onClick={() => onChange(undefined)}>
        Any
      </Pill>
      <Pill active={value === true} onClick={() => onChange(true)}>
        Yes
      </Pill>
      <Pill active={value === false} onClick={() => onChange(false)}>
        No
      </Pill>
    </div>
  )
}

function Pill({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`px-3 py-1 text-ui font-medium rounded-full border transition-colors ${
        active
          ? 'bg-warm/10 text-warm border-warm/25'
          : 'text-ink-3 border-line-1 hover:text-ink-2 hover:border-line-1/80'
      }`}
    >
      {children}
    </button>
  )
}

// --- Enum dropdown ----------------------------------------------------------

function EnumField({
  value,
  options,
  onChange,
}: {
  value: string | undefined
  options: string[]
  onChange: (v: unknown) => void
}) {
  return (
    <select
      value={value ?? ''}
      onChange={(e) => onChange(e.target.value || undefined)}
      className="w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors"
    >
      <option value="">Any</option>
      {options.map((opt) => (
        <option key={opt} value={opt}>
          {opt}
        </option>
      ))}
    </select>
  )
}

// --- Enum list: toggle pills, none on = no filter ----------------------------

function EnumListField({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: string[] | undefined
  options: string[]
  onChange: (v: unknown) => void
}) {
  const selected = value ?? []
  const toggle = (opt: string) => {
    const next = selected.includes(opt)
      ? selected.filter((v) => v !== opt)
      : [...selected, opt].sort((a, b) => options.indexOf(a) - options.indexOf(b))
    onChange(next.length > 0 ? next : undefined)
  }
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap gap-1" role="group" aria-label={label}>
        {options.map((opt) => (
          <button
            key={opt}
            type="button"
            aria-pressed={selected.includes(opt)}
            onClick={() => toggle(opt)}
            className={`px-2.5 py-0.5 text-reported font-medium rounded-full border transition-colors ${
              selected.includes(opt)
                ? 'bg-warm/10 text-warm border-warm/25'
                : 'text-ink-3 border-line-1 hover:text-ink-2 hover:border-line-1/80'
            }`}
          >
            {opt}
          </button>
        ))}
      </div>
      {selected.length === 0 && (
        <p className="text-reported text-ink-3">
          None selected: no filter on the {label.toLowerCase()}.
        </p>
      )}
    </div>
  )
}

// --- Text list: chips plus an input; Enter or a comma adds -------------------

function TextListField({
  label,
  value,
  onChange,
}: {
  label: string
  value: string[] | undefined
  onChange: (v: unknown) => void
}) {
  const selected = value ?? []
  const [draft, setDraft] = useState('')
  // Adds every non-empty entry of parts that is not already listed, ignoring
  // case, in one change.
  const commit = (...parts: string[]) => {
    const next = [...selected]
    for (const part of parts) {
      const entry = part.trim()
      if (entry && !next.some((v) => v.toLowerCase() === entry.toLowerCase())) next.push(entry)
    }
    if (next.length > selected.length) onChange(next)
  }
  const remove = (entry: string) => {
    const next = selected.filter((v) => v !== entry)
    onChange(next.length > 0 ? next : undefined)
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5 px-3 py-2 rounded-lg border border-line-1 bg-raised focus-within:border-warm/40 focus-within:ring-1 focus-within:ring-warm/20 transition-colors">
      {selected.map((entry) => (
        <span
          key={entry}
          className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border bg-tint-2 text-ink-2 border-line-2"
        >
          {entry}
          <button
            type="button"
            onClick={() => remove(entry)}
            aria-label={`Remove ${entry}`}
            className="hover:text-ink-1"
          >
            <X size={11} aria-hidden="true" />
          </button>
        </span>
      ))}
      <input
        type="text"
        aria-label={label}
        value={draft}
        onChange={(e) => {
          // A comma ends an entry, so a pasted list lands whole.
          const parts = e.target.value.split(',')
          const rest = parts.pop() ?? ''
          if (parts.length > 0) commit(...parts)
          setDraft(rest)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            commit(draft)
            setDraft('')
          } else if (e.key === 'Backspace' && draft === '' && selected.length > 0) {
            remove(selected[selected.length - 1])
          }
        }}
        onBlur={() => {
          commit(draft)
          setDraft('')
        }}
        placeholder={selected.length === 0 ? 'any; Enter adds one' : ''}
        className="flex-1 min-w-[120px] bg-transparent text-body text-ink-1 placeholder:text-ink-3 focus:outline-none"
      />
    </div>
  )
}

// --- String input -----------------------------------------------------------

function StringField({
  value,
  onChange,
  placeholder,
}: {
  value: string | undefined
  onChange: (v: unknown) => void
  placeholder?: string
}) {
  return (
    <input
      type="text"
      value={value ?? ''}
      onChange={(e) => onChange(e.target.value || undefined)}
      placeholder={placeholder ?? 'any'}
      className="w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors"
    />
  )
}

// --- Int input --------------------------------------------------------------

function IntField({
  value,
  onChange,
}: {
  value: number | undefined
  onChange: (v: unknown) => void
}) {
  return (
    <input
      type="number"
      value={value ?? ''}
      onChange={(e) => onChange(e.target.value ? Number(e.target.value) : undefined)}
      placeholder="any"
      className="w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors"
    />
  )
}

// --- Util -------------------------------------------------------------------

function humanize(name: string): string {
  return name.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase())
}
