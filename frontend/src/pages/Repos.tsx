import { useState, useEffect, useRef, useCallback, useMemo } from 'react'
import * as Popover from '@radix-ui/react-popover'
import { ChevronDown, GitBranch, Plus, RotateCw } from 'lucide-react'
import RepoPickerModal from '../components/RepoPickerModal'
import { useWebSocket } from '../hooks/useWebSocket'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'

interface RepoProfile {
  id: string
  owner: string
  repo: string
  description?: string
  has_readme: boolean
  has_claude_md: boolean
  has_agents_md: boolean
  profile_text?: string
  default_branch?: string
  base_branch?: string
  profiled_at?: string
}

// --- BranchPicker ----------------------------------------------------------
// Radix Popover instead of a hand-rolled absolutely-positioned dropdown so
// the list portals to body and isn't clipped by the card's stacking context
// (the old `backdrop-blur-xl` on the card created its own paint boundary,
// so a z-50 child could never escape it — classic z-index fail).

function BranchPicker({
  profile,
  onSave,
}: {
  profile: RepoProfile
  onSave: (branch: string) => void
}) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState(profile.base_branch || '')
  const [branches, setBranches] = useState<string[]>([])
  const [loading, setLoading] = useState(false)
  const debounceRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  useEffect(() => {
    setQuery(profile.base_branch || '')
  }, [profile.base_branch])

  const effective = profile.base_branch || profile.default_branch || 'main'
  const usingDefault = !profile.base_branch

  const fetchBranches = useCallback(
    async (q: string) => {
      setLoading(true)
      try {
        const res = await fetch(
          `/api/repos/${profile.owner}/${profile.repo}/branches?q=${encodeURIComponent(q)}`,
        )
        if (res.ok) {
          setBranches((await res.json()) as string[])
        }
      } catch {
        // non-critical — list just stays empty
      } finally {
        setLoading(false)
      }
    },
    [profile.owner, profile.repo],
  )

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (next) {
      fetchBranches(query)
    }
  }

  const handleQueryChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const v = e.target.value
    setQuery(v)
    clearTimeout(debounceRef.current)
    debounceRef.current = setTimeout(() => fetchBranches(v), 200)
  }

  const handleSelect = (branch: string) => {
    setQuery(branch)
    setOpen(false)
    onSave(branch)
  }

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      const v = query.trim()
      setOpen(false)
      if (v !== (profile.base_branch || '')) onSave(v)
    }
    if (e.key === 'Escape') {
      setOpen(false)
      setQuery(profile.base_branch || '')
    }
  }

  return (
    <Popover.Root open={open} onOpenChange={handleOpenChange}>
      <Popover.Trigger asChild>
        <button
          type="button"
          className={`group inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-[11px] transition-colors ${
            open
              ? 'bg-accent/10 text-accent'
              : 'text-text-tertiary hover:text-text-secondary hover:bg-black/[0.03]'
          }`}
        >
          <GitBranch size={11} strokeWidth={2} />
          <span className={usingDefault ? 'text-text-tertiary' : 'text-text-secondary'}>
            {effective}
          </span>
          <ChevronDown size={10} className={`transition-transform ${open ? 'rotate-180' : ''}`} />
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="end"
          sideOffset={6}
          className="z-[60] w-64 origin-top-right rounded-xl border border-border-glass bg-surface-raised/95 backdrop-blur-xl shadow-lg shadow-black/[0.08] data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=open]:zoom-in-95 data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95"
        >
          <div className="px-2 pt-2 pb-1.5 border-b border-border-subtle">
            <input
              autoFocus
              value={query}
              onChange={handleQueryChange}
              onKeyDown={handleKeyDown}
              placeholder={profile.default_branch || 'main'}
              className="w-full bg-transparent px-2 py-1 text-[12px] text-text-primary placeholder:text-text-tertiary/60 focus:outline-none"
            />
          </div>
          <div className="max-h-56 overflow-y-auto py-1">
            {loading && branches.length === 0 ? (
              <div className="px-3 py-1.5 text-[11px] text-text-tertiary">Loading…</div>
            ) : branches.length === 0 ? (
              <div className="px-3 py-1.5 text-[11px] text-text-tertiary">No branches found</div>
            ) : (
              branches.map((b) => {
                const isDefault = b === profile.default_branch
                const isCurrent = b === (profile.base_branch || '')
                return (
                  <button
                    key={b}
                    type="button"
                    onClick={() => handleSelect(b)}
                    className={`flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-[12px] transition-colors hover:bg-accent/[0.06] ${
                      isCurrent ? 'text-accent' : 'text-text-primary'
                    }`}
                  >
                    <span className="truncate">{b}</span>
                    {isDefault && (
                      <span className="shrink-0 text-[10px] text-text-tertiary">default</span>
                    )}
                  </button>
                )
              })
            )}
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

// --- StatusDot -------------------------------------------------------------
// LED indicator with three states:
//   ready     — filled accent with soft halo; docs present + profile generated
//   profiling — hollow, pulsing; docs present but summary not back yet
//   no-docs   — hollow, rust (dismiss color); profiling can never run
//
// The halo is a box-shadow rather than a filter so it stays crisp through
// the card's backdrop-blur and doesn't smear with the glass.

type DotState = 'ready' | 'profiling' | 'no-docs'

// Accessible labels for the status LED. Same string goes on title
// (sighted hover) and aria-label (screen reader / AT), so the signal
// the dot conveys visually is conveyed to every user.
const DOT_LABELS: Record<DotState, string> = {
  ready: 'Profile ready',
  profiling: 'Profiling in progress',
  'no-docs': 'No documentation files — profile cannot be generated',
}

function StatusDot({ state }: { state: DotState }) {
  const label = DOT_LABELS[state]
  if (state === 'ready') {
    return (
      <span
        role="img"
        aria-label={label}
        title={label}
        className="block h-2 w-2 shrink-0 rounded-full bg-[var(--color-accent)]"
        style={{ boxShadow: '0 0 8px 0 var(--color-accent-soft)' }}
      />
    )
  }
  if (state === 'profiling') {
    return (
      <span role="img" aria-label={label} title={label} className="relative block h-2 w-2 shrink-0">
        <span
          aria-hidden
          className="absolute inset-0 animate-ping rounded-full bg-[var(--color-accent)] opacity-50"
        />
        <span
          aria-hidden
          className="absolute inset-0 rounded-full border border-[var(--color-accent)]"
        />
      </span>
    )
  }
  // no-docs
  return (
    <span
      role="img"
      aria-label={label}
      title={label}
      className="block h-2 w-2 shrink-0 rounded-full border"
      style={{ borderColor: 'var(--color-dismiss)' }}
    />
  )
}

// --- RepoCard --------------------------------------------------------------

function RepoCard({
  profile,
  onBranchChange,
  webBaseURL,
}: {
  profile: RepoProfile
  onBranchChange: (branch: string) => void
  webBaseURL: string | undefined
}) {
  const [expanded, setExpanded] = useState(false)
  const bodyRef = useRef<HTMLParagraphElement>(null)
  const [isClamped, setIsClamped] = useState(false)

  // After render, check whether the description actually overflows the
  // clamp. Only show the "expand" affordance when there's something to
  // expand — short profiles don't get a dangling toggle.
  useEffect(() => {
    const el = bodyRef.current
    if (!el) return
    if (expanded) return
    setIsClamped(el.scrollHeight > el.clientHeight + 1)
  }, [profile.profile_text, expanded])

  const hasAnyDocs = profile.has_readme || profile.has_claude_md || profile.has_agents_md

  const state: DotState = !hasAnyDocs ? 'no-docs' : profile.profile_text ? 'ready' : 'profiling'

  return (
    <article
      className="
        group relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-5 shadow-sm shadow-black/[0.03] backdrop-blur-xl
        transition-[box-shadow,border-color] duration-300
        hover:border-white/90 hover:shadow-md hover:shadow-black/[0.05]
      "
    >
      {/* Top-left catchlight — implies refraction without being loud */}
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-white/30 blur-2xl"
      />

      {/* Header row */}
      <header className="relative flex items-center gap-3">
        <StatusDot state={state} />
        <h3 className="text-[13px] font-semibold tracking-tight text-text-primary truncate">
          {profile.id}
        </h3>
        <div className="ml-auto flex items-center gap-3">
          <BranchPicker profile={profile} onSave={onBranchChange} />
          {profile.profiled_at && (
            <span className="text-[10px] text-text-tertiary whitespace-nowrap tabular-nums">
              {formatAge(profile.profiled_at)}
            </span>
          )}
        </div>
      </header>

      {/* Recessed description well */}
      <div className="relative mt-3 rounded-xl bg-black/[0.02] ring-1 ring-inset ring-black/[0.04] px-4 py-3">
        {profile.profile_text ? (
          <>
            <p
              ref={bodyRef}
              className={`text-[12px] leading-relaxed text-text-secondary ${
                expanded ? '' : 'line-clamp-3'
              }`}
            >
              {profile.profile_text}
            </p>
            {isClamped && !expanded && (
              <>
                <div
                  aria-hidden
                  className="pointer-events-none absolute inset-x-4 bottom-0 h-5 rounded-b-xl bg-gradient-to-t from-[rgba(247,245,242,0.9)] to-transparent"
                />
                <button
                  type="button"
                  onClick={() => setExpanded(true)}
                  aria-label={`Show full profile for ${profile.id}`}
                  aria-expanded={false}
                  className="mt-1 text-[11px] font-medium text-accent/80 hover:text-accent transition-colors"
                >
                  Show more
                </button>
              </>
            )}
            {expanded && (
              <button
                type="button"
                onClick={() => setExpanded(false)}
                aria-label={`Collapse profile for ${profile.id}`}
                aria-expanded={true}
                className="mt-2 text-[11px] font-medium text-text-tertiary hover:text-text-secondary transition-colors"
              >
                Show less
              </button>
            )}
          </>
        ) : hasAnyDocs ? (
          <div className="space-y-1.5">
            <div className="h-2.5 w-full animate-pulse rounded-full bg-black/[0.05]" />
            <div className="h-2.5 w-5/6 animate-pulse rounded-full bg-black/[0.05]" />
            <div className="h-2.5 w-4/6 animate-pulse rounded-full bg-black/[0.05]" />
          </div>
        ) : (
          <p className="text-[12px] italic text-text-tertiary">
            No README, CLAUDE.md, or AGENTS.md — profile cannot be generated.
          </p>
        )}

        {/* Doc presence pinned to the well's bottom-right. Present chips
            link to the file on GitHub (default branch) — one click to see
            exactly what fed the profiling agent. */}
        {hasAnyDocs && (
          <div className="mt-3 flex items-center justify-end gap-1">
            <DocChip
              label="README"
              present={profile.has_readme}
              href={docURL(webBaseURL, profile, 'README.md')}
            />
            <DocChip
              label="CLAUDE"
              present={profile.has_claude_md}
              href={docURL(webBaseURL, profile, 'CLAUDE.md')}
            />
            <DocChip
              label="AGENTS"
              present={profile.has_agents_md}
              href={docURL(webBaseURL, profile, 'AGENTS.md')}
            />
          </div>
        )}
      </div>
    </article>
  )
}

function DocChip({ label, present, href }: { label: string; present: boolean; href?: string }) {
  if (!present) {
    return (
      <span className="rounded-full px-1.5 py-0.5 text-[9px] font-medium tracking-wide text-text-tertiary/50 line-through">
        {label}
      </span>
    )
  }
  const base =
    'rounded-full border border-accent/15 bg-accent/5 px-1.5 py-0.5 text-[9px] font-medium tracking-wide text-accent'
  if (!href) {
    return <span className={base}>{label}</span>
  }
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      title={`Open ${label === 'README' ? 'README' : label}.md on GitHub`}
      className={`${base} transition-colors hover:border-accent/35 hover:bg-accent/10`}
    >
      {label}
    </a>
  )
}

// docURL builds a web URL for a doc file at the repo's default branch,
// honoring the user's configured GitHub base URL so Enterprise installs
// open the right host. Returns undefined when webBaseURL isn't known yet —
// DocChip renders as non-clickable in that case rather than sending the
// user to a wrong destination.
function docURL(
  webBaseURL: string | undefined,
  profile: RepoProfile,
  filename: string,
): string | undefined {
  if (!webBaseURL) return undefined
  const branch = profile.default_branch || 'main'
  const root = webBaseURL.replace(/\/+$/, '') // drop trailing slash if any
  return `${root}/${profile.owner}/${profile.repo}/blob/${branch}/${filename}`
}

// deriveGitHubWebRoot converts a GitHub API URL to the corresponding web
// root. Users store the API URL in settings (what the gh client needs);
// the web UI lives at a different path on Enterprise.
//
//   https://api.github.com           → https://github.com           (GH.com)
//   https://api.github.com/          → https://github.com           (trailing slash)
//   https://github.example.com/api/v3 → https://github.example.com  (GHE)
//   https://github.com               → https://github.com           (already web)
//
// Returns undefined when the input is unparseable — caller renders
// doc chips as non-clickable in that case. We don't guess github.com
// because that's the wrong destination for Enterprise users.
function deriveGitHubWebRoot(apiURL: string): string | undefined {
  try {
    const u = new URL(apiURL)
    if (u.hostname === 'api.github.com') {
      return 'https://github.com'
    }
    const path = u.pathname.replace(/\/api\/v[34]\/?$/, '')
    return `${u.protocol}//${u.host}${path}`.replace(/\/+$/, '')
  } catch {
    return undefined
  }
}

// --- Helpers ---------------------------------------------------------------

function formatAge(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const m = Math.floor(diff / 60000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d`
  return `${Math.floor(d / 30)}mo`
}

// --- Page ------------------------------------------------------------------

export default function Repos() {
  const [profiles, setProfiles] = useState<RepoProfile[]>([])
  const [loading, setLoading] = useState(true)
  const [pickerOpen, setPickerOpen] = useState(false)
  const [selectedRepos, setSelectedRepos] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  // Starts unset — we don't know the right host until settings load. Doc
  // chips render as non-clickable text until this populates. Better than
  // briefly pointing at github.com: an Enterprise user clicking that gets
  // a broken destination, which is worse than no destination.
  const [webBaseURL, setWebBaseURL] = useState<string | undefined>(undefined)

  const fetchData = async () => {
    try {
      const res = await fetch('/api/repos')
      if (res.ok) {
        const data: RepoProfile[] = await res.json()
        setProfiles(data)
        setSelectedRepos(data.map((p) => p.id))
      }
    } catch {
      // non-critical
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    fetchData()
    fetch('/api/settings')
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => {
        const url = data?.github?.base_url
        if (typeof url === 'string' && url) {
          setWebBaseURL(deriveGitHubWebRoot(url))
        }
      })
      .catch(() => {
        // If settings fetch fails, webBaseURL stays undefined and doc
        // chips render as non-clickable — no silent wrong-destination links.
      })
  }, [])

  // Live updates from profiling pipeline
  useWebSocket((event) => {
    if (event.type === 'repo_docs_updated') {
      const d = event.data as {
        id: string
        has_readme: boolean
        has_claude_md: boolean
        has_agents_md: boolean
      }
      setProfiles((prev) =>
        prev.map((p) =>
          p.id === d.id
            ? {
                ...p,
                has_readme: d.has_readme,
                has_claude_md: d.has_claude_md,
                has_agents_md: d.has_agents_md,
              }
            : p,
        ),
      )
    }
    if (event.type === 'repo_profile_updated') {
      const d = event.data as { id: string; profile_text: string }
      setProfiles((prev) =>
        prev.map((p) => (p.id === d.id ? { ...p, profile_text: d.profile_text } : p)),
      )
    }
  })

  const handleSaveRepos = async (repos: string[]) => {
    setSaving(true)
    try {
      const res = await fetch('/api/repos', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repos }),
      })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to save repositories'))
      } else {
        toast.success('Repositories updated — profiling will run shortly')
        setSelectedRepos(repos)
        setTimeout(fetchData, 5000)
      }
    } catch (err) {
      toast.error(`Could not save repositories: ${(err as Error).message}`)
    } finally {
      setSaving(false)
      setPickerOpen(false)
    }
  }

  const handleReprofile = async () => {
    setSaving(true)
    try {
      const res = await fetch('/api/repos', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repos: selectedRepos }),
      })
      if (res.ok) {
        toast.success('Re-profiling started')
        setTimeout(fetchData, 8000)
      } else {
        toast.error(await readError(res, 'Failed to start re-profile'))
      }
    } catch (err) {
      toast.error(`Could not start re-profile: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  const handleBranchChange = (profile: RepoProfile) => async (branch: string) => {
    try {
      const res = await fetch(`/api/repos/${profile.owner}/${profile.repo}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ base_branch: branch || null }),
      })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to update base branch'))
        return
      }
      setProfiles((prev) =>
        prev.map((p) => (p.id === profile.id ? { ...p, base_branch: branch } : p)),
      )
    } catch (err) {
      toast.error(`Failed to update base branch: ${(err as Error).message}`)
    }
  }

  const profiledCount = useMemo(() => profiles.filter((p) => p.profile_text).length, [profiles])

  if (loading) {
    return (
      <div className="flex min-h-[50vh] items-center justify-center">
        <p className="text-[13px] text-text-tertiary">Loading repos…</p>
      </div>
    )
  }

  return (
    <div className="mx-auto max-w-3xl">
      {/* Etched gradient rail — subtle Halo HUD nod, warm copper fade */}
      <div
        aria-hidden
        className="mb-4 h-px w-full bg-gradient-to-r from-transparent via-[var(--color-accent-soft)] to-transparent"
      />

      <header className="mb-6 flex items-start justify-between gap-6">
        <div>
          <div className="flex items-baseline gap-2">
            <h1 className="text-[22px] font-semibold tracking-tight text-text-primary">
              Repositories
            </h1>
            {profiles.length > 0 && (
              <span className="text-[11px] tabular-nums text-text-tertiary">
                {profiledCount}/{profiles.length} profiled
              </span>
            )}
          </div>
          <p className="mt-1 text-[13px] text-text-tertiary">
            Watched repos surface in your triage queue and anchor Jira-to-code matching for
            delegation.
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <ActionButton
            icon={<RotateCw size={12} />}
            label={saving ? 'Working…' : 'Re-profile'}
            onClick={handleReprofile}
            disabled={saving || profiles.length === 0}
          />
          <ActionButton
            icon={<Plus size={12} />}
            label="Edit selection"
            onClick={() => setPickerOpen(true)}
            accent
          />
        </div>
      </header>

      {profiles.length === 0 ? (
        <EmptyState onPick={() => setPickerOpen(true)} />
      ) : (
        <div className="space-y-3">
          {profiles.map((profile) => (
            <RepoCard
              key={profile.id}
              profile={profile}
              onBranchChange={handleBranchChange(profile)}
              webBaseURL={webBaseURL}
            />
          ))}
        </div>
      )}

      {pickerOpen && (
        <RepoPickerModal
          selected={selectedRepos}
          onSave={handleSaveRepos}
          onClose={() => setPickerOpen(false)}
        />
      )}
    </div>
  )
}

// --- Small building blocks -------------------------------------------------

function ActionButton({
  icon,
  label,
  onClick,
  disabled,
  accent,
}: {
  icon: React.ReactNode
  label: string
  onClick: () => void
  disabled?: boolean
  accent?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={`
        inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-[12px] font-medium
        transition-colors disabled:opacity-40 disabled:hover:bg-transparent
        ${
          accent
            ? 'text-accent hover:bg-accent/[0.08]'
            : 'text-text-secondary hover:text-text-primary hover:bg-black/[0.03]'
        }
      `}
    >
      {icon}
      {label}
    </button>
  )
}

function EmptyState({ onPick }: { onPick: () => void }) {
  return (
    <div
      className="
        relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-12 text-center backdrop-blur-xl
      "
    >
      <span
        aria-hidden
        className="pointer-events-none absolute -left-12 -top-12 h-36 w-36 rounded-full bg-white/30 blur-2xl"
      />
      <p className="relative text-[13px] text-text-secondary">No repositories configured yet.</p>
      <p className="relative mt-1 text-[12px] text-text-tertiary">
        Pick a few to start watching for PRs and to anchor Jira delegation.
      </p>
      <button
        type="button"
        onClick={onPick}
        className="
          relative mt-5 inline-flex items-center gap-1.5 rounded-full
          border border-accent/25 px-4 py-1.5 text-[12px] font-medium text-accent
          transition-colors hover:bg-accent/[0.06] hover:border-accent/40
        "
      >
        <Plus size={12} />
        Add repositories
      </button>
    </div>
  )
}
