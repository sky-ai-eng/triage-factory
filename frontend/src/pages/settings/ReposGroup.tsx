import { useState } from 'react'
import { Plus } from 'lucide-react'
import { Section } from './primitives'
import RepoPickerModal from '../../components/RepoPickerModal'

/**
 * ReposGroup is the team-scope tracked-repositories field group. A
 * controlled component: the container owns the selected slugs (`value`) and
 * the actual PUT /api/settings/team/{id}/repos, so the same group serves the
 * team Settings tab, the create-time TeamConfigure step, and the local Setup
 * wizard. It shows the current selection and opens the shared
 * RepoPickerModal (which lists the org's GitHub repos) to edit it; picking
 * reports the new set up via onChange — this group never persists on its own.
 *
 * The richer standalone Repos page (profiling status, base-branch pickers,
 * re-profile) stays the place to manage repos in depth; this group is the
 * lightweight pick-what-to-track surface the config flows share.
 */
export default function ReposGroup({
  value,
  onChange,
  canEdit = true,
  loaded = true,
}: {
  value: string[]
  onChange: (repos: string[]) => void
  canEdit?: boolean
  // false = the team's current repos couldn't be loaded. The empty `value` is
  // then "unknown", not "you picked none" — and a save deliberately leaves
  // repos untouched — so suppress editing and say so, rather than rendering a
  // blank selection that reads as a deliberate clear.
  loaded?: boolean
}) {
  const [pickerOpen, setPickerOpen] = useState(false)

  return (
    <Section>
      <div className="flex items-center justify-between mb-1">
        <h2 className="text-[13px] font-medium text-text-secondary">Repositories</h2>
        {canEdit && loaded && (
          <button
            type="button"
            onClick={() => setPickerOpen(true)}
            className="inline-flex items-center gap-1.5 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors"
          >
            <Plus size={12} />
            {value.length > 0 ? 'Edit selection' : 'Add repositories'}
          </button>
        )}
      </div>
      <p className="text-[11px] text-text-tertiary mb-3 leading-relaxed">
        Watched repos surface in this team&rsquo;s triage queue and anchor Jira-to-code matching for
        delegation.
      </p>

      {!loaded ? (
        <p className="text-[12px] text-amber-600 italic">
          Couldn&rsquo;t load this team&rsquo;s repositories — they&rsquo;ll be left unchanged.
          Reload to edit them.
        </p>
      ) : value.length === 0 ? (
        <p className="text-[12px] text-text-tertiary italic">No repositories selected yet.</p>
      ) : (
        <div className="flex flex-wrap gap-1.5">
          {value.map((slug) => (
            <span
              key={slug}
              className="inline-flex items-center rounded-full bg-accent-soft text-accent px-2.5 py-0.5 text-[11px] font-mono"
            >
              {slug}
            </span>
          ))}
        </div>
      )}

      {pickerOpen && (
        <RepoPickerModal
          selected={value}
          onSave={(repos) => {
            onChange(repos)
            setPickerOpen(false)
          }}
          onClose={() => setPickerOpen(false)}
        />
      )}
    </Section>
  )
}
