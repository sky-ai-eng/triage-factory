import { useState, useEffect, useMemo, useCallback, useRef } from 'react'
import { createPortal } from 'react-dom'
import { RotateCw, ExternalLink } from 'lucide-react'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID, getGitHubAppStatus, getGitHubAppInstallURL } from '../lib/githubApp'
import SearchField from './SearchField'

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
  /** If provided, shows a Back button in inline mode */
  onBack?: () => void
  /** Pre-fetched repo list — skips the /api/github/repos fetch if provided */
  cachedRepos?: GitHubRepo[]
  /** Called with fetched repos so the parent can cache them */
  onReposFetched?: (repos: GitHubRepo[]) => void
  /**
   * True while the parent's onSave (the team-repos PUT) is in flight. Disables
   * the Continue/Save button and swaps in a spinner so a slow save on a large
   * GHES org reads as "working" rather than "broken" (SKY-409). Distinct from
   * the component's internal repo-list fetch `loading`.
   */
  saving?: boolean
  /**
   * Hide the footer (the selected-count + Save/Continue/Back buttons). For
   * hosts that drive navigation and persistence themselves — the setup wizard
   * embeds the picker inline and uses its own Continue/Back, so the picker's
   * footer would just duplicate them. Pair with onSelectionChange to read the
   * live selection.
   */
  hideFooter?: boolean
  /**
   * Fired with the full selection whenever it changes — and once on mount with
   * the seed. Lets a host that hides the footer mirror the selection into its
   * own state (the wizard persists from there on its own Continue). Re-reporting
   * the seed is idempotent for the wizard (it patches the same value it passed
   * in), so the host stays in sync without a separate read.
   */
  onSelectionChange?: (repos: string[]) => void
}

export type { GitHubRepo }

export default function RepoPickerModal({
  selected,
  onSave,
  onClose,
  inline,
  onBack,
  cachedRepos,
  onReposFetched,
  saving = false,
  hideFooter = false,
  onSelectionChange,
}: Props) {
  const [repos, setRepos] = useState<GitHubRepo[]>(cachedRepos ?? [])
  const [loading, setLoading] = useState(!cachedRepos)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')
  const [checked, setChecked] = useState<Set<string>>(new Set(selected))

  // Report the live selection up to a footer-less host (the setup wizard) so it
  // can mirror the picks into its own state and persist them on its own
  // Continue. Held in a ref so this doesn't depend on a callback identity that
  // changes each render. Idempotent by design: re-reporting the seed (on mount,
  // or after the host re-renders) just re-sets the same value, and the host
  // never persists an empty set without an explicit pick — so this can't loop
  // or silently wipe an unloaded selection.
  const onSelectionChangeRef = useRef(onSelectionChange)
  onSelectionChangeRef.current = onSelectionChange
  useEffect(() => {
    onSelectionChangeRef.current?.(Array.from(checked))
  }, [checked])

  // Under a GitHub App, the picker lists the repos the App is installed on
  // (GET /installation/repositories) — not arbitrary user repos — so the copy
  // and empty state change, and we offer a deep-link to install the App on
  // more repositories. PAT-only orgs keep the original copy. The org-scoped App
  // endpoints take the id in the path; local mode uses the sentinel org.
  const auth = useOptionalAuth()
  const isLocal = auth === null
  const activeOrgId = useActiveOrgId()
  const orgId = isLocal ? LOCAL_DEFAULT_ORG_ID : activeOrgId
  const [appMode, setAppMode] = useState(false)
  const [installUrl, setInstallUrl] = useState('')

  useEffect(() => {
    if (!orgId) return
    let cancelled = false
    getGitHubAppStatus(orgId)
      .then((status) => {
        if (cancelled) return
        // App mode = the org runs on its own registered App with at least one
        // installation, which is exactly when the backend sources the picker
        // from installation repositories.
        const inApp = !!status.app && status.installations.length > 0
        setAppMode(inApp)
        if (inApp) {
          // getGitHubAppInstallURL validates the scheme (http/https only) at
          // the source and rejects anything else, so installUrl is safe to use
          // directly as an href below.
          getGitHubAppInstallURL(orgId)
            .then((url) => {
              if (!cancelled) setInstallUrl(url)
            })
            .catch(() => {})
        }
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [orgId])

  const fetchRepos = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const res = await fetch('/api/github/repos')
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        console.error('Failed to fetch repos:', data.error || `HTTP ${res.status}`)
        // TFAC-324's distinct 400: an active App installed on zero accounts (and
        // no PAT to borrow) dead-ends the picker here. Point the user at the
        // install affordance rather than the generic "couldn't fetch" copy.
        if (data.error === 'GitHub App is not installed on any account') {
          setError(
            'Your GitHub App isn’t installed on any account yet. Install it from the “Install the App” step (or the App installation section in Settings), then try again.',
          )
        } else {
          setError('Failed to fetch repositories')
        }
        return
      }
      const data: GitHubRepo[] = await res.json()
      setRepos(data)
      onReposFetched?.(data)
    } catch (err) {
      console.error('Failed to fetch repos:', err)
      setError('Failed to fetch repositories')
    } finally {
      setLoading(false)
    }
  }, [onReposFetched])

  useEffect(() => {
    if (!cachedRepos) fetchRepos()
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  const filtered = useMemo(() => {
    if (!search.trim()) return repos
    const q = search.toLowerCase()
    return repos.filter(
      (r) =>
        r.full_name.toLowerCase().includes(q) ||
        (r.description || '').toLowerCase().includes(q) ||
        (r.language || '').toLowerCase().includes(q),
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
    <div className={`flex flex-col ${inline ? '' : 'h-full max-h-[80vh]'}`}>
      {/* Header */}
      <div className={inline ? 'pb-4' : 'px-6 pt-6 pb-4'}>
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Select repositories
        </h2>
        <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
          {appMode
            ? 'These are the repositories your GitHub App is installed on. Choose which to watch — PRs from them appear in your triage queue, and Jira tickets are matched to them for delegation.'
            : 'Choose which repos to watch. PRs from these repos appear in your triage queue, and Jira tickets are matched to these repos for delegation.'}
        </p>
        {appMode && installUrl && (
          <a
            href={installUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors mt-2"
          >
            Install the App on more repositories
            <ExternalLink size={12} />
          </a>
        )}
      </div>

      {/* Search */}
      <div className={inline ? 'pb-3' : 'px-6 pb-3'}>
        {inline ? (
          <SearchField
            value={search}
            onChange={setSearch}
            placeholder="Search repos…"
            ariaLabel="Search repositories"
          />
        ) : (
          <input
            type="text"
            placeholder="Search repos..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors"
          />
        )}
      </div>

      {/* List — capped to a sensible height inline (the wizard); the overlay
          fills its modal. */}
      <div className={`overflow-y-auto min-h-0 ${inline ? 'max-h-[22rem]' : 'flex-1 px-6'}`}>
        {loading && (
          <div className="space-y-1 py-2">
            {[1, 2, 3, 4, 5, 6, 7, 8].map((i) => (
              <div key={i} className="flex items-center gap-3 px-3 py-2.5 rounded-xl">
                <div className="w-4 h-4 rounded bg-black/[0.04] animate-pulse" />
                <div className="flex-1 space-y-1.5">
                  <div
                    className="h-3 rounded bg-black/[0.04] animate-pulse"
                    style={{ width: `${55 + ((i * 17) % 35)}%` }}
                  />
                  <div
                    className="h-2.5 rounded bg-black/[0.03] animate-pulse"
                    style={{ width: `${30 + ((i * 23) % 40)}%` }}
                  />
                </div>
              </div>
            ))}
          </div>
        )}

        {error && (
          <div className="flex flex-col items-center justify-center py-12 gap-3">
            <div className="text-[13px] text-text-secondary text-center">{error}</div>
            <button
              type="button"
              onClick={fetchRepos}
              className="flex items-center gap-1.5 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors"
            >
              <RotateCw size={13} />
              Retry
            </button>
          </div>
        )}

        {!loading && !error && filtered.length === 0 && (
          <div className="flex flex-col items-center justify-center gap-3 py-8">
            <p className="text-[13px] text-text-tertiary text-center">
              {search
                ? `No repos match "${search}"`
                : appMode
                  ? "Your GitHub App isn't installed on any repositories yet."
                  : 'No repositories found'}
            </p>
            {!search && appMode && installUrl && (
              <a
                href={installUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors"
              >
                Install the App on repositories
                <ExternalLink size={12} />
              </a>
            )}
          </div>
        )}

        {!loading &&
          !error &&
          filtered.map((repo) => {
            const isChecked = checked.has(repo.full_name)
            return (
              <button
                key={repo.full_name}
                type="button"
                role="checkbox"
                aria-checked={isChecked}
                onClick={() => toggle(repo.full_name)}
                className={`w-full flex items-start gap-3 px-3 py-2.5 text-left rounded-xl transition-colors hover:bg-black/[0.02] ${
                  isChecked ? 'bg-accent/[0.04]' : ''
                }`}
              >
                <span
                  className={`mt-0.5 shrink-0 w-4 h-4 rounded border flex items-center justify-center transition-colors ${
                    isChecked ? 'bg-accent border-accent text-white' : 'border-border-subtle'
                  }`}
                >
                  {isChecked && (
                    <svg
                      width="10"
                      height="10"
                      viewBox="0 0 10 10"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    >
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

      {/* Footer — omitted when the host (the setup wizard) drives its own
          navigation + persistence and just reads the selection via
          onSelectionChange. A standing selected-count keeps the host honest. */}
      {!hideFooter && (
        <div
          className={`flex items-center justify-between border-t border-border-subtle py-4 ${
            inline ? 'mt-2' : 'px-6'
          }`}
        >
          <span className="text-[12px] text-text-tertiary">
            {checked.size} repo{checked.size !== 1 ? 's' : ''} selected
          </span>
          <div className="flex gap-3">
            {inline && onBack && (
              <button
                type="button"
                onClick={onBack}
                className="text-[13px] text-text-secondary hover:text-text-primary bg-white/50 hover:bg-white/80 border border-border-subtle rounded-xl px-4 py-2 transition-colors"
              >
                Back
              </button>
            )}
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
              disabled={checked.size === 0 || saving}
              className="flex items-center gap-1.5 bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-5 py-2 text-[13px] transition-colors"
            >
              {saving && <RotateCw size={13} className="animate-spin" />}
              {saving ? 'Saving…' : inline ? 'Continue' : 'Save'}
            </button>
          </div>
        </div>
      )}
    </div>
  )

  if (inline) {
    // Flush in the wizard — no card chrome, the content flows on the step like
    // everything else. The list caps its own height (above), so it stays in the
    // flow rather than ballooning.
    return <div className="w-full">{content}</div>
  }

  // Portal to <body>: the overlay is position:fixed, but the field groups
  // render inside a Section whose `backdrop-blur-xl` establishes a containing
  // block for fixed descendants — without the portal the "modal" is trapped
  // and clipped inside the Repositories card instead of covering the viewport.
  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/20 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        className="w-full max-w-xl backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.06] overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {content}
      </div>
    </div>,
    document.body,
  )
}
