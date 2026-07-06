import { useRef } from 'react'
import { Lock, Users, Globe } from 'lucide-react'
import { nextRadioIndex } from '../lib/rovingRadio'
import type { ProjectVisibility } from '../types'

// ProjectVisibilitySelect (TFAC-562) is a 3-option radiogroup — private /
// team / org — with options grayed out based on what the viewer can
// actually do, rather than letting a choice they can't back up 40x at
// submit time. "team" is unavailable to a teamless viewer; "org" is
// unavailable to anyone but an org admin (mirrors the projects_insert /
// projects_update RLS policies exactly — org visibility requires
// tf.user_is_org_admin, team visibility requires tf.user_can_write_team).
// Multi-mode only: callers should not render this at all in local mode.
export default function ProjectVisibilitySelect({
  value,
  onChange,
  canTeam,
  canOrg,
  canPrivate = true,
  className = '',
}: {
  value: ProjectVisibility
  onChange: (v: ProjectVisibility) => void
  /** Whether "team" is selectable — false for a viewer on zero teams (or,
   *  on an existing project's edit view, a project with no team_id). */
  canTeam: boolean
  /** Whether "org" is selectable — false for anyone but an org admin. */
  canOrg: boolean
  /** Whether "private" is selectable — defaults true (the common create-
   *  time case: the viewer is always the project's own creator-to-be).
   *  The edit view passes false for anyone but the project's creator. */
  canPrivate?: boolean
  className?: string
}) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])

  const options: {
    value: ProjectVisibility
    label: string
    hint: string
    disabledHint?: string
    icon: React.ComponentType<{ size?: number; className?: string }>
  }[] = [
    {
      value: 'private',
      label: 'Private',
      hint: 'Only you',
      disabledHint: canPrivate ? undefined : 'Only the creator can set this',
      icon: Lock,
    },
    {
      value: 'team',
      label: 'Team',
      hint: 'Your team can see and edit it',
      disabledHint: canTeam ? undefined : 'Join a team to enable this',
      icon: Users,
    },
    {
      value: 'org',
      label: 'Org-wide',
      hint: 'Everyone in the org can see it',
      disabledHint: canOrg ? undefined : 'Org admins only',
      icon: Globe,
    },
  ]

  const isEnabled = (i: number) => !options[i].disabledHint
  const selectedIndex = options.findIndex((o) => o.value === value)
  const tabbable = selectedIndex < 0 ? 0 : selectedIndex

  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, options.length, isEnabled)
    if (next === null) return
    e.preventDefault()
    onChange(options[next].value)
    btnRefs.current[next]?.focus()
  }

  return (
    <div
      role="radiogroup"
      aria-label="Project visibility"
      onKeyDown={onKeyDown}
      className={`grid grid-cols-3 gap-2 ${className}`}
    >
      {options.map((opt, i) => {
        const selected = opt.value === value
        const disabled = !!opt.disabledHint
        const Icon = opt.icon
        return (
          <button
            key={opt.value}
            ref={(el) => {
              btnRefs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={selected}
            aria-disabled={disabled}
            title={opt.disabledHint}
            tabIndex={i === tabbable ? 0 : -1}
            onClick={() => {
              if (disabled) return
              onChange(opt.value)
            }}
            className={`
              flex flex-col items-start gap-0.5 rounded-lg border px-3 py-2 text-left
              transition-colors outline-none
              ${
                disabled
                  ? 'cursor-not-allowed border-border-subtle bg-black/[0.02] opacity-50'
                  : selected
                    ? 'border-accent bg-accent-soft'
                    : 'border-border-subtle bg-white/60 hover:bg-white'
              }
            `}
          >
            <span
              className={`inline-flex items-center gap-1.5 text-[12.5px] font-medium ${
                selected && !disabled ? 'text-accent' : 'text-text-primary'
              }`}
            >
              <Icon size={12} />
              {opt.label}
            </span>
            <span className="text-[11px] leading-snug text-text-tertiary">
              {opt.disabledHint ?? opt.hint}
            </span>
          </button>
        )
      })}
    </div>
  )
}
