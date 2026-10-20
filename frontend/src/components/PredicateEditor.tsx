import { useEffect, useState } from 'react'
import { Tooltip } from '../ui/tooltip/Tooltip'
import { Info } from 'lucide-react'
import type { FieldSchema } from '../types'
import IdentityListField, { type IdentityKind } from './IdentityListField'
import SlackChannelListField from './SlackChannelListField'
import RepoListField from './RepoListField'
import LinearTeamListField from './LinearTeamListField'
import { apiJSON, httpErrorMessage } from '../lib/apiClient'

interface PredicateEditorProps {
  eventType: string
  /** The handler's team, whose tracked Slack channels, repositories and
   *  Linear teams `channel_in`, `repo_in` and `linear_team_id_in` offer.
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
      {/* Every other string_list field is an identity allowlist:
          GitHub → author_in / reviewer_in / commenter_in;
          Jira → assignee_in / reporter_in / commenter_in;
          Linear → assignee_in / creator_in / commenter_in.
          IdentityListField branches on identityKind to read the right
          column (github_username, jira_account_id or linear_user_id)
          from /api/me and the team roster. */}
      {field.type === 'string_list' && !REFERENCE_LIST_FIELDS.has(field.name) && (
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

// REFERENCE_LIST_FIELDS are the string_list fields that name rows the team
// tracks, each with its own picker, rather than people.
const REFERENCE_LIST_FIELDS = new Set(['channel_in', 'repo_in', ...LINEAR_TEAM_FIELDS])

// identityKindFor picks the identity an allowlist holds from the event's
// source prefix: a Linear assignee is a Linear user id, never a GitHub
// login.
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
