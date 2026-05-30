import { useState, useRef, useEffect } from 'react'
import { Check, ChevronDown, Pin, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'
import { toast } from './Toast/toastStore'

interface Props {
  /** The selected team id, or '' for "all my teams". */
  value: string
  onChange: (teamId: string) => void
  className?: string
}

// TeamScopeSelect is the per-page read filter. It renders
// nothing unless the viewer belongs to ≥2 teams — the same count gate
// that covers local + hosted-solo with no mode branch. Optional by
// design: "All my teams" (value '') is the union, selecting one narrows
// the view (the chosen id rides every read as ?team_id=).
//
// The pin toggles the viewer's sticky default: pinning a team persists it
// as the per-user default that seeds both this filter and the write
// picker on the next visit; pinning while on "All my teams" clears it.
export default function TeamScopeSelect({ value, onChange, className = '' }: Props) {
  const { teams, multi, preferredTeamId, setPreferred } = useTeams()
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])

  if (!multi) return null

  const selected = teams.find((t) => t.id === value)
  const label = selected ? selected.name : 'All my teams'

  const pinDefault = async () => {
    try {
      // Pin the current selection as the sticky default; "all" clears it.
      await setPreferred(value)
      toast.success(
        value ? `Default team set to ${selected?.name ?? 'team'}` : 'Default team cleared',
      )
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to set default team')
    }
  }

  return (
    <div ref={ref} className={`relative inline-flex items-center gap-1 ${className}`}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="
          inline-flex items-center gap-1.5 rounded-lg border border-border-subtle
          bg-white/60 px-2.5 py-1 text-[12px] text-text-primary
          hover:bg-white transition-colors
        "
        title="Filter by team"
      >
        <Users size={13} className="text-text-tertiary" />
        <span className="max-w-[12rem] truncate">{label}</span>
        <ChevronDown size={13} className="text-text-tertiary" />
      </button>
      <button
        type="button"
        onClick={pinDefault}
        className={`rounded-md p-1 transition-colors hover:bg-black/[0.04] ${
          value && value === preferredTeamId ? 'text-accent' : 'text-text-tertiary'
        }`}
        title={
          value === preferredTeamId && value
            ? 'This is your default team'
            : value
              ? 'Set as your default team'
              : 'Clear your default team'
        }
      >
        <Pin size={13} className={value && value === preferredTeamId ? 'fill-current' : ''} />
      </button>

      {open && (
        <div
          className="
            absolute top-full left-0 z-30 mt-1 min-w-[14rem] rounded-lg border border-border-subtle
            bg-white shadow-lg py-1
          "
        >
          <Row
            label="All my teams"
            sublabel="union of every team you're on"
            active={value === ''}
            onClick={() => {
              onChange('')
              setOpen(false)
            }}
          />
          <div className="my-1 h-px bg-border-subtle" />
          {teams.map((t) => (
            <Row
              key={t.id}
              label={t.name}
              sublabel={t.id === preferredTeamId ? 'your default' : undefined}
              active={value === t.id}
              onClick={() => {
                onChange(t.id)
                setOpen(false)
              }}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function Row({
  label,
  sublabel,
  active,
  onClick,
}: {
  label: string
  sublabel?: string
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-[12px] hover:bg-black/[0.03]"
    >
      <span className="flex flex-col">
        <span className="text-text-primary truncate">{label}</span>
        {sublabel && <span className="text-[10px] text-text-tertiary">{sublabel}</span>}
      </span>
      {active && <Check size={13} className="shrink-0 text-accent" />}
    </button>
  )
}
