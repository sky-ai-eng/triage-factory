import { useEffect, useState } from 'react'
import * as Tooltip from '@radix-ui/react-tooltip'
import { Info } from 'lucide-react'
import type { FieldSchema } from '../types'

interface PredicateEditorProps {
  eventType: string
  value: Record<string, unknown>
  onChange: (value: Record<string, unknown>) => void
}

export default function PredicateEditor({ eventType, value, onChange }: PredicateEditorProps) {
  const [fields, setFields] = useState<FieldSchema[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!eventType) {
      setFields([])
      return
    }
    let cancelled = false
    setLoading(true)
    setError('')

    fetch(`/api/event-schemas/${encodeURIComponent(eventType)}`)
      .then((r) => {
        if (!r.ok) throw new Error(`Failed to load schema for ${eventType}`)
        return r.json()
      })
      .then((data) => {
        if (!cancelled) {
          setFields(data.fields || [])
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err.message)
          setFields([])
          setLoading(false)
        }
      })

    // Reset predicate when event type changes.
    onChange({})

    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- onChange is stable, eventType is the trigger
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
    return <p className="text-[12px] text-text-tertiary italic">Select an event type first.</p>
  }

  if (loading) {
    return (
      <div className="space-y-3">
        {[1, 2, 3].map((i) => (
          <div key={i} className="h-10 rounded-lg bg-black/[0.03] animate-pulse" />
        ))}
      </div>
    )
  }

  if (error) {
    return <p className="text-[12px] text-dismiss">{error}</p>
  }

  if (fields.length === 0) {
    return (
      <p className="text-[12px] text-text-tertiary italic">
        No filterable fields for this event type.
      </p>
    )
  }

  return (
    <div className="space-y-3">
      {fields.map((field) => (
        <FieldRow
          key={field.name}
          field={field}
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
  value: unknown
  onChange: (val: unknown) => void
}

function FieldRow({ field, value, onChange }: FieldRowProps) {
  return (
    <div>
      <div className="flex items-center gap-1.5 mb-1.5">
        <label className="text-[12px] font-medium text-text-secondary">
          {humanize(field.name)}
        </label>
        {field.description && (
          <Tooltip.Root>
            <Tooltip.Trigger asChild>
              <Info size={12} className="text-text-tertiary cursor-help" />
            </Tooltip.Trigger>
            <Tooltip.Portal>
              <Tooltip.Content
                sideOffset={5}
                className="max-w-[260px] px-3 py-2 rounded-lg bg-text-primary text-white text-[11px] leading-relaxed shadow-lg z-[100]"
              >
                {field.description}
                <Tooltip.Arrow className="fill-text-primary" />
              </Tooltip.Content>
            </Tooltip.Portal>
          </Tooltip.Root>
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
      {field.type === 'string_list' && (
        <StringField
          value={value as string | undefined}
          onChange={onChange}
          placeholder="comma-separated values"
        />
      )}
    </div>
  )
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
      className={`px-3 py-1 text-[12px] font-medium rounded-full border transition-colors ${
        active
          ? 'bg-accent/10 text-accent border-accent/25'
          : 'text-text-tertiary border-border-subtle hover:text-text-secondary hover:border-border-subtle/80'
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
      className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
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
      className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
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
      className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
    />
  )
}

// --- Util -------------------------------------------------------------------

function humanize(name: string): string {
  return name.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase())
}
