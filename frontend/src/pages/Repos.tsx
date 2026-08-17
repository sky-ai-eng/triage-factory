import { useState, useEffect, useRef, useCallback, useMemo } from 'react'
import * as Popover from '@radix-ui/react-popover'
import { ChevronDown, GitBranch, Plus, RotateCw, AlertTriangle } from 'lucide-react'
import { Link } from 'react-router'
import RepoPickerModal from '../components/RepoPickerModal'
import { useOrgHref } from '../hooks/useOrgHref'
import { useApiOrgId } from '../hooks/useApiOrgId'
import { useWebSocket } from '../hooks/useWebSocket'
import { toast } from '../components/Toast/toastStore'
import { apiFetch, apiJSON, apiList, httpErrorMessage } from '../lib/apiClient'

// Repo tracking is per-team; writes go to the team's repos
// endpoint. This page is pre-team-context, so it targets the org's
// default team — a future change will thread the selected team here.
// The default team's repo *tracking* set — read to seed the selection and
// written on Save/Re-profile. This must NOT be sourced from GET /api/repos
// (the org-wide repositories union): in a multi-team org that union
// includes sibling teams' repos, and writing it back here would pull them
// into the default team's tracked set and past the router gate.
const TEAM_REPOS_PATH = '/api/settings/team/default/repos'

interface Repository {
  /** The registry row id. Identity: the React key, the websocket merge
   *  key, and the path segment every repo route is addressed by. GitHub
   *  cannot change it, so a rename mid-session neither strands a card nor
   *  404s a save. */
  id: string
  /** "owner/repo" — the display name, and only that. The server renders
   *  it (domain.Repository.Slug()); this page reads it. */
  slug: string
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
  // Bare-clone status reported by the worktree package via main.go's
  // SetOnCloneResult hook. 'pending' for repos that haven't been
  // attempted yet (legacy rows or repos selected since the last
  // bootstrap pass); 'ok' / 'failed' for attempted repos.
  clone_status?: 'ok' | 'failed' | 'pending'
  clone_error?: string
  // 'ssh' when our SSH preflight confirmed the SSH side is the cause
  // of the failure (so the UI can offer "Fix in Settings"); 'other'
  // when the failure is on the git/transport side (show raw stderr,
  // no settings shortcut).
  clone_error_kind?: 'ssh' | 'other'
  // Whether this caller may change the repo's settings. The server
  // decides — a repository row is org-wide, so writing it takes an org
  // admin or a team admin of a team tracking it, and only the server
  // knows which of the tracking teams the caller administers. Never
  // re-derive this from the viewer's role. Absent (older server) reads
  // as false, which degrades to read-only rather than to a control that
  // 403s on use.
  can_edit?: boolean
}

// The branch a run actually targets: an explicit base_branch override,
// else the profiler-derived default, else GitHub's own fallback. Shared
// by the editable picker and the read-only label so the two can't drift.
function effectiveBranch(profile: Repository): string {
  return profile.base_branch || profile.default_branch || 'main'
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
  profile: Repository
  onSave: (branch: string) => void
}) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState(profile.base_branch || '')
  const [branches, setBranches] = useState<string[]>([])
  const [loading, setLoading] = useState(false)
  const debounceRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const mountedRef = useRef(true)
  // Current in-flight fetch's AbortController. Each new fetchBranches call
  // aborts the previous one so out-of-order resolution can't clobber the
  // list with stale results (user types "ma" then "main"; "ma" resolves
  // later and overwrites "main"'s results without this guard).
  const abortRef = useRef<AbortController | null>(null)

  // Cleanup on unmount: drop any pending debounce timer, abort any in-flight
  // fetch, and mark the component unmounted so anything that still resolves
  // against stale references doesn't setState on a dead tree.
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      if (debounceRef.current !== undefined) {
        clearTimeout(debounceRef.current)
        debounceRef.current = undefined
      }
      abortRef.current?.abort()
      abortRef.current = null
    }
  }, [])

  useEffect(() => {
    setQuery(profile.base_branch || '')
  }, [profile.base_branch])

  const effective = effectiveBranch(profile)
  const usingDefault = !profile.base_branch

  const fetchBranches = useCallback(
    async (q: string) => {
      if (!mountedRef.current) return
      // Cancel the previous in-flight so its late resolution can't overwrite
      // the newer request's results, and save network work.
      abortRef.current?.abort()
      const controller = new AbortController()
      abortRef.current = controller

      setLoading(true)
      try {
        // One page is what the picker shows: it is a type-to-filter box, and
        // the filter is what narrows the list, not scrolling it.
        const page = await apiList<{ name: string }>(
          `/api/repos/${encodeURIComponent(profile.id)}/branches/list`,
          { q, page_size: 30 },
          { signal: controller.signal },
        )
        if (mountedRef.current) setBranches(page.items.map((b) => b.name))
      } catch (err) {
        // AbortError is expected when a newer request supersedes this one;
        // the superseding call already set loading=true and will handle
        // state. Any other error is non-critical — list stays as-is.
        if ((err as { name?: string })?.name === 'AbortError') return
      } finally {
        // Only clear loading if this request wasn't superseded — otherwise
        // we'd flash the spinner off between two rapid keystrokes even
        // though a newer fetch is still in flight.
        if (!controller.signal.aborted && mountedRef.current) {
          setLoading(false)
        }
      }
    },
    [profile.id],
  )

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (next) {
      fetchBranches(query)
      return
    }

    const v = query.trim()
    if (v !== (profile.base_branch || '')) onSave(v)
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
                // Compare against the effective branch (same fallback chain
                // as the trigger chip), not raw base_branch — otherwise
                // when the user has no override set, nothing highlights
                // even though a branch IS effectively selected.
                const isCurrent = b === effective
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

// --- BranchLabel -----------------------------------------------------------
// The read-only twin of BranchPicker's trigger chip, shown when the caller
// may not change this repo's settings. Same glyph and typography so the row
// still reads as "this repo targets <branch>" — it just isn't a control.
// Deliberately not a disabled <button>: there's no action to offer, and a
// dead button invites the click that would 403.

function BranchLabel({ profile }: { profile: Repository }) {
  return (
    <span
      title="Only an org admin, or a team admin of a team tracking this repo, can change the base branch"
      className="inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-[11px] text-text-tertiary"
    >
      <GitBranch size={11} strokeWidth={2} />
      <span className={profile.base_branch ? 'text-text-secondary' : 'text-text-tertiary'}>
        {effectiveBranch(profile)}
      </span>
    </span>
  )
}

// --- StatusDot -------------------------------------------------------------
// LED indicator with four states:
//   ready     — filled accent with soft halo; docs present + profile generated
//   profiling — hollow, pulsing; docs present but summary not back yet
//   pending   — hollow, muted; not yet inspected (skeleton row awaiting the
//               first profiler pass, or a repo whose doc fetch errored and was
//               left for retry — TFAC-331)
//   no-docs   — hollow, rust (dismiss color); inspected, genuinely no docs
//
// The halo is a box-shadow rather than a filter so it stays crisp through
// the card's backdrop-blur and doesn't smear with the glass.

type DotState = 'ready' | 'profiling' | 'pending' | 'no-docs'

// Accessible labels for the status LED. Same string goes on title
// (sighted hover) and aria-label (screen reader / AT), so the signal
// the dot conveys visually is conveyed to every user.
const DOT_LABELS: Record<DotState, string> = {
  ready: 'Profile ready',
  profiling: 'Profiling in progress',
  pending: 'Profiling pending',
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
  if (state === 'pending') {
    // Muted hollow dot — distinct from the rust no-docs dot: this repo
    // hasn't been inspected yet (or is queued for retry), not a dead end.
    return (
      <span
        role="img"
        aria-label={label}
        title={label}
        className="block h-2 w-2 shrink-0 rounded-full border"
        style={{ borderColor: 'var(--color-text-tertiary)' }}
      />
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

// --- CloneFailedBadge ------------------------------------------------------
// A small red pill anchored next to the repo title, with a Radix Popover
// that exposes the actual error and a CTA. The CTA branches on
// clone_error_kind: SSH-classified failures get a "Fix in Settings"
// link (the user almost certainly needs to set up keys / agent / known
// hosts); other failures just show the raw stderr so the user can
// diagnose. We deliberately do NOT try to summarize git stderr — the
// text is short enough to display verbatim and the user is the right
// audience to interpret git/curl/connection errors.

function CloneFailedBadge({ profile }: { profile: Repository }) {
  const [open, setOpen] = useState(false)
  const orgHref = useOrgHref()
  const isSSH = profile.clone_error_kind === 'ssh'
  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
      <Popover.Trigger asChild>
        <button
          type="button"
          aria-label={`Clone failed for ${profile.slug}`}
          className="
            inline-flex items-center gap-1 rounded-full
            border border-[var(--color-dismiss)]/30
            bg-[var(--color-dismiss)]/10 px-2 py-0.5
            text-[10px] font-medium uppercase tracking-wide
            text-[var(--color-dismiss)]
            hover:bg-[var(--color-dismiss)]/15 transition-colors
          "
        >
          <AlertTriangle size={10} />
          Clone failed
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          side="bottom"
          align="start"
          sideOffset={6}
          className="
            z-50 w-[320px] rounded-xl border border-border-glass
            bg-surface-raised/95 backdrop-blur-xl shadow-lg shadow-black/[0.08]
            p-3 text-[12px] text-text-primary
          "
        >
          <p className="font-semibold mb-1.5">
            {isSSH ? 'SSH not configured for GitHub' : 'Clone failed'}
          </p>
          {isSSH ? (
            <p className="text-text-secondary leading-snug mb-2">
              The bare-clone for this repo couldn&apos;t be created over SSH. Our preflight against
              your GitHub host also failed — check that your SSH key is added to GitHub and loaded
              into your agent, or switch the clone protocol to HTTPS.
            </p>
          ) : (
            <p className="text-text-secondary leading-snug mb-2">
              The bare-clone for this repo failed. Raw output from git:
            </p>
          )}
          {profile.clone_error && (
            <pre
              className="
              mb-2 max-h-[140px] overflow-auto rounded
              bg-black/[0.04] p-2 text-[11px] text-text-secondary
              whitespace-pre-wrap break-words
            "
            >
              {profile.clone_error}
            </pre>
          )}
          {isSSH && (
            <Link
              to={orgHref('/settings')}
              onClick={() => setOpen(false)}
              className="
                inline-flex items-center gap-1 rounded-md
                bg-accent/10 px-2 py-1 text-[12px] font-medium
                text-accent hover:bg-accent/15 transition-colors
              "
            >
              Fix in Settings
            </Link>
          )}
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

// --- RepoCard --------------------------------------------------------------

function RepoCard({
  profile,
  onBranchChange,
  webBaseURL,
}: {
  profile: Repository
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
  // A row with no docs AND no default_branch was never successfully
  // inspected: either profiling hasn't run yet, or its doc fetch errored and
  // the row was left untouched for retry (TFAC-331). The profiler only
  // populates default_branch after a successful repo-meta fetch, so its
  // presence is the "this repo was actually checked" signal — it separates a
  // pending skeleton from a genuinely doc-less repo without a new schema column.
  const neverChecked = !hasAnyDocs && !profile.default_branch

  const state: DotState = hasAnyDocs
    ? profile.profile_text
      ? 'ready'
      : 'profiling'
    : neverChecked
      ? 'pending'
      : 'no-docs'

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
          {profile.slug}
        </h3>
        {profile.clone_status === 'failed' && <CloneFailedBadge profile={profile} />}
        <div className="ml-auto flex items-center gap-3">
          {profile.can_edit ? (
            <BranchPicker profile={profile} onSave={onBranchChange} />
          ) : (
            <BranchLabel profile={profile} />
          )}
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
              <button
                type="button"
                onClick={() => setExpanded(true)}
                aria-label={`Show full profile for ${profile.slug}`}
                aria-expanded={false}
                className="mt-1 text-[11px] font-medium text-accent/80 hover:text-accent transition-colors"
              >
                Show more
              </button>
            )}
            {expanded && (
              <button
                type="button"
                onClick={() => setExpanded(false)}
                aria-label={`Collapse profile for ${profile.slug}`}
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
        ) : neverChecked ? (
          <p className="text-[12px] italic text-text-tertiary">Profiling pending…</p>
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
  profile: Repository,
  filename: string,
): string | undefined {
  if (!webBaseURL) return undefined
  const branch = profile.default_branch || 'main'
  const root = webBaseURL.replace(/\/+$/, '') // drop trailing slash if any
  return `${root}/${profile.owner}/${profile.repo}/blob/${branch}/${filename}`
}

// sanitizeWebRoot validates and normalizes the stored GitHub base URL for
// use as a link href. Settings stores the user-facing web root already —
// "https://github.com" or "https://github.example.com" — because that's
// what internal/github.NewClient takes (it derives the API base from it
// internally). So no API→web conversion is needed here; we just trim a
// trailing slash and reject obviously malformed input.
//
// Returns undefined when the input isn't a plausible http(s) URL — caller
// renders doc chips as non-clickable rather than building hrefs from
// junk that would 404 or worse.
function sanitizeWebRoot(url: string): string | undefined {
  const trimmed = url.trim().replace(/\/+$/, '')
  if (!/^https?:\/\/\S+/i.test(trimmed)) {
    return undefined
  }
  return trimmed
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
  const apiOrgId = useApiOrgId()
  const [profiles, setProfiles] = useState<Repository[]>([])
  const [loading, setLoading] = useState(true)
  const [pickerOpen, setPickerOpen] = useState(false)
  const [selectedRepos, setSelectedRepos] = useState<string[]>([])
  // True when the default team's tracked-set fetch failed. selectedRepos
  // stays [] on failure, which is indistinguishable from "genuinely tracks
  // nothing" — without this flag a transient failure would let Re-profile
  // PUT an empty array and wipe the team's repos (+ GC `repositories`).
  const [teamLoadFailed, setTeamLoadFailed] = useState(false)
  const [saving, setSaving] = useState(false)
  // Starts unset — we don't know the right host until settings load. Doc
  // chips render as non-clickable text until this populates. Better than
  // briefly pointing at github.com: an Enterprise user clicking that gets
  // a broken destination, which is worse than no destination.
  const [webBaseURL, setWebBaseURL] = useState<string | undefined>(undefined)

  const fetchData = async () => {
    try {
      // Two distinct lists: the cards show the org-wide repositories union
      // (the registry list), but the *selection* — what Save/Re-profile writes
      // back to the default team — must come from the default team's own
      // tracked set, never the union (see TEAM_REPOS_PATH).
      // Each settles on its own: the cards are worth painting even when the
      // team read fails, and a failed team read must not be mistaken for an
      // empty tracked set (see setTeamLoadFailed below).
      const [profiles, team] = await Promise.all([
        apiList<Repository>('/api/repos/list', { page_size: 200 })
          .then((page) => page.items)
          .catch(() => null),
        apiJSON<{ repos?: string[] }>(TEAM_REPOS_PATH).catch(() => null),
      ])
      if (profiles) setProfiles(profiles)
      if (team) {
        setSelectedRepos(team.repos ?? [])
        setTeamLoadFailed(false)
      } else {
        // Don't leave a stale-or-empty selection that a write could
        // clobber the team with — mark the load as failed so destructive
        // actions stay disabled until a successful refetch.
        setTeamLoadFailed(true)
      }
    } catch {
      setTeamLoadFailed(true)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    fetchData()
    if (!apiOrgId) return
    apiJSON<{ github_base_url?: string }>(`/api/orgs/${encodeURIComponent(apiOrgId)}/settings`)
      .then((data) => {
        const url = data?.github_base_url
        if (typeof url === 'string' && url) {
          setWebBaseURL(sanitizeWebRoot(url))
        }
      })
      .catch(() => {
        // If settings fetch fails, webBaseURL stays undefined and doc
        // chips render as non-clickable — no silent wrong-destination links.
      })
  }, [apiOrgId])

  // Live updates from the profiling pipeline + clone-result hook. All
  // three producers (doc scan, AI profiler, clone status) share one
  // sparse-diff event: merge whichever fields are present rather than
  // overwriting — a clone-status diff must not blank the AI profile_text
  // and vice versa.
  useWebSocket((event) => {
    if (event.type === 'repository_updated') {
      const d = event.data
      setProfiles((prev) =>
        prev.map((p) => {
          if (p.id !== d.id) return p
          const next: Repository = { ...p }
          if (d.has_readme !== undefined) next.has_readme = d.has_readme
          if (d.has_claude_md !== undefined) next.has_claude_md = d.has_claude_md
          if (d.has_agents_md !== undefined) next.has_agents_md = d.has_agents_md
          if (d.profile_text !== undefined) next.profile_text = d.profile_text
          if (d.clone_status !== undefined) next.clone_status = d.clone_status
          if (d.clone_error !== undefined) next.clone_error = d.clone_error
          if (d.clone_error_kind !== undefined) next.clone_error_kind = d.clone_error_kind
          return next
        }),
      )
    }
  })

  const handleSaveRepos = async (repos: string[]) => {
    setSaving(true)
    try {
      await apiFetch(TEAM_REPOS_PATH, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repos }),
      })
      toast.success('Repositories updated — profiling will run shortly')
      setSelectedRepos(repos)
      setTimeout(fetchData, 5000)
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not save the repositories.'))
    } finally {
      setSaving(false)
      setPickerOpen(false)
    }
  }

  const handleReprofile = async () => {
    // Re-profile re-PUTs the current selection. Never do that when the
    // selection is empty or its load failed — that would clear the team's
    // tracked repos. The button is also disabled in these states; this is
    // the defense-in-depth guard.
    if (teamLoadFailed || selectedRepos.length === 0) {
      return
    }
    setSaving(true)
    try {
      await apiFetch(TEAM_REPOS_PATH, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repos: selectedRepos }),
      })
      toast.success('Re-profiling started')
      setTimeout(fetchData, 8000)
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not start the re-profile.'))
    } finally {
      setSaving(false)
    }
  }

  const handleBranchChange = (profile: Repository) => async (branch: string) => {
    try {
      // The PATCH answers with the updated row, so the card renders what the
      // server stored rather than what this call sent — and without a refetch
      // of the whole list to find out. The two are the same for base_branch
      // today; they are not for a field the write normalizes, and the row is
      // the half that is true either way.
      const updated = await apiJSON<Repository>(`/api/repos/${encodeURIComponent(profile.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ base_branch: branch || null }),
      })
      setProfiles((prev) => prev.map((p) => (p.id === updated.id ? updated : p)))
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not update the base branch.'))
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
          {teamLoadFailed && (
            <p className="mt-1 text-[13px] text-amber-600">
              Couldn&rsquo;t load your repo selection — editing and re-profiling are paused to avoid
              overwriting it.{' '}
              <button
                type="button"
                onClick={() => fetchData()}
                className="underline hover:text-amber-700"
              >
                Retry
              </button>
            </p>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <ActionButton
            icon={<RotateCw size={12} />}
            label={saving ? 'Working…' : 'Re-profile'}
            onClick={handleReprofile}
            // Gate on the *selection* (what gets PUT), not the profile
            // cards: an empty selection re-PUTs nothing meaningful, and a
            // failed selection-load must not be written back as empty.
            disabled={saving || teamLoadFailed || selectedRepos.length === 0}
          />
          <ActionButton
            icon={<Plus size={12} />}
            label="Edit selection"
            onClick={() => setPickerOpen(true)}
            // Disabled while the selection failed to load: the picker would
            // open showing an empty selection, and saving could drop repos
            // the team actually tracks. Retry first.
            disabled={teamLoadFailed}
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
