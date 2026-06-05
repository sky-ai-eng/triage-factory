import { Section, Field, inputClass } from './primitives'

interface TeamSettingsValue {
  default_model: string
  auto_delegate_enabled: boolean
}

/**
 * TeamSettingsGroup is the team-scope AI field group: the delegation model
 * default and the auto-delegation toggle. A controlled component — the
 * container owns the form state and the POST /api/settings/team/{id} — so
 * the same group serves the team Settings tab, the create-time TeamConfigure
 * step, and (forthcoming) the local Setup wizard without a parallel
 * implementation.
 *
 * The team's AI thresholds (reprioritize threshold / preference-update
 * interval) are part of the same team_settings payload and round-trip
 * through teamConfig's saver, but no control exposes them yet, so they're
 * deliberately absent here rather than invented — their UI is a later
 * ticket.
 */
export default function TeamSettingsGroup({
  value,
  onChange,
}: {
  value: TeamSettingsValue
  onChange: (patch: Partial<TeamSettingsValue>) => void
}) {
  return (
    <Section>
      <h2 className="text-[13px] font-medium text-text-secondary mb-4">AI</h2>
      <div className="space-y-3">
        <Field label="Delegation model">
          <select
            value={value.default_model}
            onChange={(e) => onChange({ default_model: e.target.value })}
            className={inputClass}
          >
            <option value="haiku">Haiku (fast, cheap)</option>
            <option value="sonnet">Sonnet (balanced)</option>
            <option value="opus">Opus (most capable)</option>
          </select>
        </Field>
        <div className="flex items-center justify-between">
          <div>
            <p className="text-[13px] text-text-primary">Auto-delegation</p>
            <p className="text-[11px] text-text-tertiary mt-0.5">
              Automatically delegate tasks when matching triggers fire
            </p>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={value.auto_delegate_enabled}
            onClick={() => onChange({ auto_delegate_enabled: !value.auto_delegate_enabled })}
            className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
              value.auto_delegate_enabled ? 'bg-accent' : 'bg-black/[0.08]'
            }`}
          >
            <span
              className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-sm transform transition-transform ${
                value.auto_delegate_enabled ? 'translate-x-4' : 'translate-x-0'
              }`}
            />
          </button>
        </div>
      </div>
    </Section>
  )
}
