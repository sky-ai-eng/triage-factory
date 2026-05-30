import { useState, useEffect, useCallback, useRef } from 'react'
import { X } from 'lucide-react'
import type { Project } from '../types'
import { readError } from '../lib/api'
import { toast } from './Toast/toastStore'
import RepoMultiSelect from './RepoMultiSelect'
import TrackerProjectPickers from './TrackerProjectPickers'
import TeamPicker from './TeamPicker'
import { useWriteTeam, noteWrittenTeam } from '../hooks/useTeams'

// ProjectCreateModal is the only way to create a project from the UI.
// Required: name. Optional: description, pinned repos, tracker
// projects (Jira / Linear). The pinned-repos picker filters to the
// configured-repos list (server validates anyway, but pre-filtering
// keeps the user from picking a slug that's about to be rejected).
interface Props {
  onClose: () => void
  onCreated: (project: Project) => void
}

export default function ProjectCreateModal({ onClose, onCreated }: Props) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [pinnedRepos, setPinnedRepos] = useState<string[]>([])
  const [jiraKey, setJiraKey] = useState('')
  const [linearKey, setLinearKey] = useState('')
  const [submitting, setSubmitting] = useState(false)
  // Acting team. The TeamPicker renders only at ≥2 teams; below that
  // `team` stays '' and the server resolves the sole team. `ready` is
  // false until /api/teams resolves — the submit button gates on it so a
  // multi-team user can't submit team_id:'' in the cold-load window.
  const { team, setTeam, ready: teamReady } = useWriteTeam()

  // Switching teams invalidates the pinned-repo selection: repos are
  // team-scoped (validatePinnedRepos checks the acting team's tracked
  // set), so a repo picked under team A would 400 when submitting under
  // team B. Reset to empty and let the user re-pick from the new team.
  const onTeamChange = useCallback(
    (next: string) => {
      setTeam(next)
      setPinnedRepos([])
    },
    [setTeam],
  )
  // Holds the in-flight POST's AbortController so the close path
  // can cancel it. Without this, clicking the backdrop / hitting
  // Escape after submit would dismiss the dialog while leaving the
  // request in flight — and a successful response after dismissal
  // would silently create a project the user thinks they cancelled,
  // tempting a duplicate retry.
  const abortRef = useRef<AbortController | null>(null)

  // Escape closes unless a create request is in flight. We could
  // alternatively allow Escape to abort the request explicitly, but
  // "block close while submitting" is the simpler contract — typical
  // create takes <500ms locally, the user can wait.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, submitting])

  // If the modal unmounts mid-flight (e.g. parent forces close),
  // abort the request so we don't end up with a phantom project.
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!name.trim()) {
        toast.error('Name is required')
        return
      }
      setSubmitting(true)
      const controller = new AbortController()
      abortRef.current = controller
      try {
        const res = await fetch('/api/projects', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name: name.trim(),
            description: description.trim(),
            pinned_repos: pinnedRepos,
            jira_project_key: jiraKey,
            linear_project_key: linearKey,
            team_id: team,
          }),
          signal: controller.signal,
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to create project'))
          return
        }
        const created: Project = await res.json()
        // Sync the cache to the team this write landed on (the backend
        // resolver has already persisted it as the last-written default).
        if (team) noteWrittenTeam(team)
        toast.success(`Created project "${created.name}"`)
        onCreated(created)
      } catch (err) {
        // Ignore abort — the close path that triggered it is
        // responsible for any user-facing feedback.
        if ((err as { name?: string })?.name === 'AbortError') return
        toast.error(`Failed to create project: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        abortRef.current = null
        setSubmitting(false)
      }
    },
    [name, description, pinnedRepos, jiraKey, linearKey, team, onCreated],
  )

  // Backdrop click closes only when no submit is in flight. If a
  // submit is in flight, ignoring the click is safer than trying
  // to abort + close — the user might have clicked accidentally,
  // and a half-second wait is cheaper than a wrongly-cancelled
  // create that already wrote a row.
  const handleBackdropClick = () => {
    if (submitting) return
    onClose()
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={handleBackdropClick}
    >
      <div
        className="
          relative w-full max-w-lg max-h-[90vh] overflow-y-auto
          rounded-2xl border border-border-glass
          bg-gradient-to-br from-white/95 via-white/90 to-white/85
          shadow-xl shadow-black/[0.08] backdrop-blur-xl
          p-6
        "
        role="dialog"
        aria-modal="true"
        aria-labelledby="project-create-modal-title"
        aria-describedby="project-create-modal-description"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="flex items-start justify-between mb-5">
          <div>
            <h2
              id="project-create-modal-title"
              className="text-lg font-semibold tracking-tight text-text-primary"
            >
              New project
            </h2>
            <p
              id="project-create-modal-description"
              className="text-[12px] text-text-tertiary mt-0.5"
            >
              You can add or change everything later.
            </p>
          </div>
          <button
            type="button"
            onClick={() => {
              if (!submitting) onClose()
            }}
            disabled={submitting}
            className="text-text-tertiary hover:text-text-secondary p-1 rounded-full hover:bg-black/[0.03] disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:text-text-tertiary disabled:hover:bg-transparent"
            aria-label="Close"
          >
            <X size={16} />
          </button>
        </header>

        <form onSubmit={handleSubmit} className="space-y-4">
          <TeamPicker value={team} onChange={onTeamChange} label="Team" />

          <Field label="Name" required>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoFocus
              required
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/60 px-3 py-2 text-[13px] text-text-primary
                placeholder:text-text-tertiary
                focus:outline-none focus:border-accent focus:bg-white
              "
              placeholder="e.g. Triage Factory"
            />
          </Field>

          <Field label="Description">
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/60 px-3 py-2 text-[13px] text-text-primary
                placeholder:text-text-tertiary resize-none
                focus:outline-none focus:border-accent focus:bg-white
              "
              placeholder="What this project is about (optional)"
            />
          </Field>

          <Field label="Pinned repos">
            <RepoMultiSelect value={pinnedRepos} onChange={setPinnedRepos} teamId={team} />
          </Field>

          <Field label="Tracker projects">
            <TrackerProjectPickers
              jiraKey={jiraKey}
              linearKey={linearKey}
              onJiraChange={setJiraKey}
              onLinearChange={setLinearKey}
            />
          </Field>

          <div className="flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={onClose}
              disabled={submitting}
              className="
                rounded-full px-4 py-2 text-[13px]
                text-text-secondary hover:text-text-primary hover:bg-black/[0.03]
                transition-all disabled:opacity-50
              "
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting || !name.trim() || !teamReady}
              className="
                rounded-full px-4 py-2 text-[13px] font-medium
                bg-accent text-white hover:opacity-90
                disabled:opacity-50 transition-all
              "
            >
              {submitting ? 'Creating…' : 'Create project'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

function Field({
  label,
  required,
  children,
}: {
  label: string
  required?: boolean
  children: React.ReactNode
}) {
  return (
    <label className="block">
      <span className="block text-[12px] font-medium text-text-secondary mb-1.5">
        {label}
        {required && <span className="text-accent ml-0.5">*</span>}
      </span>
      {children}
    </label>
  )
}
