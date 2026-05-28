export type SettingsTab = 'my' | 'team' | 'workspace'

const TABS: { id: SettingsTab; label: string }[] = [
  { id: 'my', label: 'My Settings' },
  { id: 'team', label: 'Team' },
  { id: 'workspace', label: 'Workspace' },
]

/** Tab navigation for the multi-member Settings layout. Collapsed away in
 *  the N=1 case (the page renders flat). My-scope holds device/personal
 *  preferences (theme today; user_settings fields later) — server-scoped
 *  PATs and AI policy live under Workspace and Team respectively. */
export default function SettingsTabs({
  tab,
  onChange,
}: {
  tab: SettingsTab
  onChange: (t: SettingsTab) => void
}) {
  return (
    <div
      role="tablist"
      aria-label="Settings scope"
      className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5 mb-6"
    >
      {TABS.map((t) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          aria-selected={tab === t.id}
          onClick={() => onChange(t.id)}
          className={`px-4 py-1.5 text-[13px] font-medium rounded-md transition-colors ${
            tab === t.id
              ? 'bg-white text-text-primary shadow-sm'
              : 'text-text-tertiary hover:text-text-secondary'
          }`}
        >
          {t.label}
        </button>
      ))}
    </div>
  )
}
