import { useState, useEffect, useMemo } from 'react'

interface GitHubRepo {
  full_name: string
  html_url: string
  description: string
  language: string
  pushed_at: string
  private: boolean
}

interface Props {
  /** Initially selected repo full_names */
  selected: string[]
  /** Called with the new selection when user clicks Save */
  onSave: (repos: string[]) => void
  /** Called when user dismisses without saving */
  onClose: () => void
  /** If true, renders as a full-page step instead of an overlay */
  inline?: boolean
}

export default function RepoPickerModal({ selected, onSave, onClose, inline }: Props) {
  const [repos, setRepos] = useState<GitHubRepo[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')
  const [checked, setChecked] = useState<Set<string>>(new Set(selected))

  useEffect(() => {
    fetchRepos()
  }, [])

  const fetchRepos = async () => {
    setLoading(true)
    setError('')
    try {
      const res = await fetch('/api/github/repos')
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to fetch repos')
        return
      }
      const data: GitHubRepo[] = await res.json()
      setRepos(data)
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const filtered = useMemo(() => {
    if (!search.trim()) return repos
    const q = search.toLowerCase()
    return repos.filter(
      (r) =>
        r.full_name.toLowerCase().includes(q) ||
        (r.description || '').toLowerCase().includes(q) ||
        (r.language || '').toLowerCase().includes(q)
    )
  }, [repos, search])

  const toggle = (name: string) => {
    setChecked((prev) => {
      const next = new Set(prev)
      if (next.has(name)) next.delete(name)
      else next.add(name)
      return next
    })
  }

  const content = (
    <div className="flex flex-col h-full max-h-[80vh]">
      {/* Header */}
      <div className="px-6 pt-6 pb-4">
        <h2 className="text-[18px] font-semibold text-text-primary tracking-tight">
          Select Repositories
        </h2>
        <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
          Choose which repos to watch. PRs from these repos appear in your triage queue,
          and Jira tickets are matched to these repos for delegation.
        </p>
      </div>

      {/* Search */}
      <div className="px-6 pb-3">
        <input
          type="text"
          placeholder="Search repos..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors"
        />
      </div>

      {/* List */}
      <div className="flex-1 overflow-y-auto px-6 min-h-0">
        {loading && (
          <div className="flex items-center justify-center py-12">
            <p className="text-[13px] text-text-tertiary">Loading repositories...</p>
          </div>
        )}

        {error && (
          <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
            {error}
          </div>
        )}

        {!loading && !error && filtered.length === 0 && (
          <p className="text-[13px] text-text-tertiary text-center py-8">
            {search ? `No repos match "${search}"` : 'No repositories found'}
          </p>
        )}

        {!loading && !error && filtered.map((repo) => {
          const isChecked = checked.has(repo.full_name)
          return (
            <button
              key={repo.full_name}
              type="button"
              onClick={() => toggle(repo.full_name)}
              className={`w-full flex items-start gap-3 px-3 py-2.5 text-left rounded-xl transition-colors hover:bg-black/[0.02] ${
                isChecked ? 'bg-accent/[0.04]' : ''
              }`}
            >
              <span className={`mt-0.5 shrink-0 w-4 h-4 rounded border flex items-center justify-center transition-colors ${
                isChecked
                  ? 'bg-accent border-accent text-white'
                  : 'border-border-subtle'
              }`}>
                {isChecked && (
                  <svg width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                    <polyline points="2 5 4 7 8 3" />
                  </svg>
                )}
              </span>
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="text-[12.5px] font-medium text-text-primary truncate">
                    {repo.full_name}
                  </span>
                  {repo.private && (
                    <span className="text-[9px] text-text-tertiary border border-border-subtle rounded px-1 py-0.5">
                      private
                    </span>
                  )}
                  {repo.language && (
                    <span className="text-[10px] text-text-tertiary">{repo.language}</span>
                  )}
                </div>
                {repo.description && (
                  <p className="text-[11px] text-text-tertiary truncate mt-0.5">
                    {repo.description}
                  </p>
                )}
              </div>
            </button>
          )
        })}
      </div>

      {/* Footer */}
      <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between">
        <span className="text-[12px] text-text-tertiary">
          {checked.size} repo{checked.size !== 1 ? 's' : ''} selected
        </span>
        <div className="flex gap-3">
          {!inline && (
            <button
              type="button"
              onClick={onClose}
              className="text-[13px] text-text-secondary hover:text-text-primary border border-border-subtle rounded-xl px-4 py-2 transition-colors"
            >
              Cancel
            </button>
          )}
          <button
            type="button"
            onClick={() => onSave(Array.from(checked))}
            disabled={checked.size === 0}
            className="bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-5 py-2 text-[13px] transition-colors"
          >
            {inline ? 'Continue' : 'Save'}
          </button>
        </div>
      </div>
    </div>
  )

  if (inline) {
    return (
      <div className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.04] overflow-hidden">
        {content}
      </div>
    )
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/20 backdrop-blur-sm" onClick={onClose}>
      <div
        className="w-full max-w-xl backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.06] overflow-hidden"
        onClick={(e) => e.stopPropagation()}>
        {content}
      </div>
    </div>
  )
}
