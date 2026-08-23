import { useState, useEffect, useId, useCallback, useRef } from 'react'
import { createPortal } from 'react-dom'
import { RotateCw, ExternalLink } from 'lucide-react'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID, getGitHubAppStatus, getGitHubAppInstallURL } from '../lib/githubApp'
import { listReachableRepos, refreshReachableRepos } from '../lib/githubRepos'
import type { GitHubRepo, RepoListStatus } from '../lib/githubRepos'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { useWebSocket } from '../hooks/useWebSocket'
import SearchField from './SearchField'
import { httpErrorMessage } from '../lib/apiClient'

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
  /**
   * True while the parent's onSave (the team-repos PUT) is in flight. Disables
   * the Continue/Save button and swaps in a spinner so a slow save on a large
   * GHES org reads as "working" rather than "broken". Distinct from
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

/** How long to wait after the last keystroke before asking the server for the
 *  filtered page. The filter is a round trip now rather than an array scan, so
 *  a request per character would be both wasteful and out of order. */
const SEARCH_DEBOUNCE_MS = 250

/** How long the refresh control keeps spinning without hearing back. The
 *  refresh is out of band and announces itself with a websocket ping, so this is
 *  only the floor under a refresh that fails server-side and never pings —
 *  long enough to cover a real enumeration of a large account. */
const REFRESH_SPINNER_TIMEOUT_MS = 30_000

/** How often to re-ask while the workspace has never been looked at. Only the
 *  discovering state polls — a loaded picker waits for the ping — so this is a
 *  backstop for a first open whose websocket ping never lands, not a substitute
 *  for it. */
const DISCOVERY_POLL_MS = 4_000

export default function RepoPickerModal({
  selected,
  onSave,
  onClose,
  inline,
  onBack,
  saving = false,
  hideFooter = false,
  onSelectionChange,
}: Props) {
  const [repos, setRepos] = useState<GitHubRepo[]>([])
  const [total, setTotal] = useState(0)
  const [nextToken, setNextToken] = useState('')
  const [listStatus, setListStatus] = useState<RepoListStatus>('ready')
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')
  const [checked, setChecked] = useState<Set<string>>(new Set(selected))

  // Trap keyboard focus inside the dialog while it's the floating overlay
  // (never in `inline` mode, which is embedded page content, not a modal)
  // and restore it to the trigger on close (WCAG 2.1.2); initial focus lands
  // on the search field.
  const dialogRef = useRef<HTMLDivElement>(null)
  const searchRef = useRef<HTMLInputElement>(null)
  const titleId = useId()
  useFocusTrap(dialogRef, { active: !inline, initialFocus: searchRef })

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

  // Whether the server has ever looked at this workspace's reachable set. It is
  // a discriminator on the response rather than "items is empty", because an
  // empty list would read as "you have no repositories" — a different and
  // alarming claim.
  const discovering = listStatus === 'discovering'

  // The query the held page was fetched under. loadMore pairs the server's
  // token with it, because a token is only valid for the search it was minted
  // for — keeping the two together in one ref is what stops them ever pairing
  // across a keystroke.
  const loadedQuery = useRef('')

  // The query the user is asking RIGHT NOW, which is not the same thing. The
  // refetches that fire on their own schedule — the discovery poll, the
  // websocket ping — must re-ask this rather than the last-loaded one: while a
  // search is still in flight those differ, and re-asking the older question
  // would let it win on arrival and leave the list disagreeing with the search
  // box. Written during render, like the selection callback above.
  const activeQuery = useRef('')
  activeQuery.current = search.trim()

  // Which first-page request the held state belongs to. Searches are typed
  // faster than they resolve and the discovery poll fires on its own schedule,
  // so several are legitimately in flight at once and they do NOT come back in
  // order — a slow request for "ac" landing after a fast one for "acme" would
  // otherwise overwrite the newer answer with the older one, and its `finally`
  // would clear `loading` while the newer request was still running. Every
  // request takes a sequence number and only the latest one is allowed to touch
  // state; the rest resolve and are dropped.
  const requestSeq = useRef(0)

  const fetchPage = useCallback(async (q: string) => {
    const seq = ++requestSeq.current
    setLoading(true)
    setError('')
    try {
      const page = await listReachableRepos(q)
      if (seq !== requestSeq.current) return
      setRepos(page.items)
      setTotal(page.total_count)
      setNextToken(page.next_page_token)
      setListStatus(page.status)
      loadedQuery.current = q
    } catch (err) {
      if (seq !== requestSeq.current) return
      console.error('Failed to fetch repos:', err)
      // TFAC-324's distinct fault: an active App installed on zero accounts (and
      // no PAT to borrow) dead-ends the picker here. Point the user at the
      // install affordance rather than the generic "couldn't fetch" copy.
      const message = httpErrorMessage(err, 'Failed to fetch repositories')
      setError(
        message === 'the GitHub App is not installed on any account'
          ? 'Your GitHub App isn’t installed on any account yet. Install it from the “Install the App” step (or the App installation section in Settings), then try again.'
          : 'Failed to fetch repositories',
      )
    } finally {
      // Superseded requests leave `loading` alone too: the request that
      // superseded them set it and is still running.
      if (seq === requestSeq.current) setLoading(false)
    }
  }, [])

  const loadMore = useCallback(async () => {
    if (!nextToken || loading) return
    // A further page takes a sequence number from the same counter as a first
    // page: appending page 2 of the previous search onto the results of a new
    // one would produce a list that matches neither.
    const seq = ++requestSeq.current
    setLoading(true)
    try {
      const page = await listReachableRepos(loadedQuery.current, nextToken)
      if (seq !== requestSeq.current) return
      setRepos((prev) => [...prev, ...page.items])
      setTotal(page.total_count)
      setNextToken(page.next_page_token)
    } catch (err) {
      if (seq !== requestSeq.current) return
      // Keep what is on screen: blanking the list because one further page
      // failed is worse than showing a short one with a retry still offered.
      setError(httpErrorMessage(err, 'Failed to fetch more repositories'))
    } finally {
      if (seq === requestSeq.current) setLoading(false)
    }
  }, [nextToken, loading])

  // Debounced search → first page. Also the mount fetch (the initial search is
  // empty), which is why there is no separate on-mount effect.
  useEffect(() => {
    const id = setTimeout(() => fetchPage(activeQuery.current), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(id)
  }, [search, fetchPage])

  // The one live spinner deadline. Held in a ref and always cleared before a new
  // one is armed, because two of them are two ways to be wrong: an earlier
  // timeout would stop the spinner for a LATER refresh that is still running,
  // and one surviving unmount would set state on a component that is gone.
  const refreshTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const clearRefreshTimer = useCallback(() => {
    if (refreshTimer.current !== null) {
      clearTimeout(refreshTimer.current)
      refreshTimer.current = null
    }
  }, [])
  useEffect(() => clearRefreshTimer, [clearRefreshTimer])

  // The server refreshes the reachable set out of band — on a first-ever open,
  // on the refresh control, and whenever GitHub credentials change — and says
  // so with a payload-free ping. Refetching on it is what turns "discovering
  // repositories…" into a list without the user reloading.
  useWebSocket(
    useCallback(
      (event) => {
        if (event.type !== 'github_repos_updated') return
        // The ping is the spinner's real end, so retire its deadline with it.
        clearRefreshTimer()
        setRefreshing(false)
        fetchPage(activeQuery.current)
      },
      [fetchPage, clearRefreshTimer],
    ),
  )

  // The ping is the fast path, not the only one. While the workspace has never
  // been looked at there is nothing on screen at all, so this state needs to
  // resolve even where the ping doesn't land — a socket that reconnected mid
  // refresh, or the setup wizard on a page whose socket isn't up yet. Polls only
  // in the discovering state, so a loaded picker costs nothing.
  useEffect(() => {
    if (!discovering) return
    const id = setInterval(() => fetchPage(activeQuery.current), DISCOVERY_POLL_MS)
    return () => clearInterval(id)
  }, [discovering, fetchPage])

  const onRefresh = useCallback(async () => {
    clearRefreshTimer()
    setRefreshing(true)
    setError('')
    try {
      await refreshReachableRepos()
      // The refresh runs out of band, so the ping is what normally ends this
      // spinner. Time it out anyway: a refresh that fails server-side (an
      // expired credential, a GitHub outage) never pings, and a control stuck on
      // "Refreshing…" forever is a worse lie than one that just stops.
      refreshTimer.current = setTimeout(() => {
        refreshTimer.current = null
        setRefreshing(false)
      }, REFRESH_SPINNER_TIMEOUT_MS)
    } catch (err) {
      setRefreshing(false)
      setError(httpErrorMessage(err, 'Could not refresh the repository list.'))
    }
  }, [clearRefreshTimer])

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
        <h2 id={titleId} className="text-[19px] font-medium tracking-tight text-ink-1">
          Select repositories
        </h2>
        <p className="text-body text-ink-3 mt-1 leading-relaxed">
          {appMode
            ? 'These are the repositories your GitHub App is installed on. Choose which to watch — PRs from them appear in your triage queue, and Jira tickets are matched to them for delegation.'
            : 'Choose which repos to watch. PRs from these repos appear in your triage queue, and Jira tickets are matched to these repos for delegation.'}
        </p>
        {appMode && installUrl && (
          <a
            href={installUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-ui font-medium text-warm hover:text-warm/80 transition-colors mt-2"
          >
            Install the App on more repositories
            <ExternalLink size={12} />
          </a>
        )}
      </div>

      {/* Search — server-side, so this narrows the whole reachable set rather
          than the page already in the DOM. */}
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
            ref={searchRef}
            type="text"
            placeholder="Search repos..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="w-full bg-raised border border-line-1 rounded-xl px-4 py-2.5 text-body text-ink-1 placeholder-ink-3 focus:outline-none focus:ring-2 focus:ring-warm/30 focus:border-warm/40 transition-colors"
          />
        )}
        {/* The count is renderable at all because the list is served from a
            local mirror with a real total — a proxy list could only ever say
            "showing N". The refresh beside it is the answer to "I just granted
            the App another repo and it isn't here", which is otherwise a wait. */}
        <div className="flex items-center justify-between mt-2">
          <span className="text-[11px] text-ink-3">
            {discovering
              ? 'Discovering repositories…'
              : `Showing ${repos.length} of ${total} repositor${total === 1 ? 'y' : 'ies'}`}
          </span>
          <button
            type="button"
            onClick={onRefresh}
            disabled={refreshing}
            className="flex items-center gap-1.5 text-[11px] font-medium text-ink-3 hover:text-ink-1 disabled:opacity-40 transition-colors"
          >
            <RotateCw size={11} className={refreshing ? 'animate-spin' : ''} />
            {refreshing ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>
      </div>

      {/* List — capped to a sensible height inline (the wizard); the overlay
          fills its modal. */}
      <div className={`overflow-y-auto min-h-0 ${inline ? 'max-h-[22rem]' : 'flex-1 px-6'}`}>
        {(loading || discovering) && repos.length === 0 && !error && (
          <div className="space-y-1 py-2">
            {[1, 2, 3, 4, 5, 6, 7, 8].map((i) => (
              <div key={i} className="flex items-center gap-3 px-3 py-2.5 rounded-xl">
                <div className="w-4 h-4 rounded bg-tint-3 animate-pulse" />
                <div className="flex-1 space-y-1.5">
                  <div
                    className="h-3 rounded bg-tint-3 animate-pulse"
                    style={{ width: `${55 + ((i * 17) % 35)}%` }}
                  />
                  <div
                    className="h-2.5 rounded bg-tint-2 animate-pulse"
                    style={{ width: `${30 + ((i * 23) % 40)}%` }}
                  />
                </div>
              </div>
            ))}
          </div>
        )}

        {error && (
          <div className="flex flex-col items-center justify-center py-12 gap-3">
            <div className="text-body text-ink-2 text-center">{error}</div>
            <button
              type="button"
              onClick={() => fetchPage(search.trim())}
              className="flex items-center gap-1.5 text-ui font-medium text-warm hover:text-warm/80 transition-colors"
            >
              <RotateCw size={13} />
              Retry
            </button>
          </div>
        )}

        {/* Empty, and we know it is empty. The discovering state is handled by
            the skeleton above, so reaching here means the server answered a
            READY page with nothing in it — which is a real answer about the
            workspace rather than a list that has not loaded. */}
        {!loading && !discovering && !error && repos.length === 0 && (
          <div className="flex flex-col items-center justify-center gap-3 py-8">
            <p className="text-body text-ink-3 text-center">
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
                className="inline-flex items-center gap-1 text-ui font-medium text-warm hover:text-warm/80 transition-colors"
              >
                Install the App on repositories
                <ExternalLink size={12} />
              </a>
            )}
          </div>
        )}

        {!error &&
          repos.map((repo) => {
            const isChecked = checked.has(repo.full_name)
            return (
              <button
                key={repo.full_name}
                type="button"
                role="checkbox"
                aria-checked={isChecked}
                onClick={() => toggle(repo.full_name)}
                className={`w-full flex items-start gap-3 px-3 py-2.5 text-left rounded-xl transition-colors hover:bg-tint-2 ${
                  isChecked ? 'bg-warm/[0.04]' : ''
                }`}
              >
                <span
                  className={`mt-0.5 shrink-0 w-4 h-4 rounded border flex items-center justify-center transition-colors ${
                    isChecked ? 'bg-warm border-warm text-warm-ink' : 'border-line-1'
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
                    <span className="text-card-title font-medium text-ink-1 truncate">
                      {repo.full_name}
                    </span>
                    {repo.private && (
                      <span className="text-label-sm text-ink-3 border border-line-1 rounded px-1 py-0.5">
                        private
                      </span>
                    )}
                    {repo.language && (
                      <span className="text-label text-ink-3">{repo.language}</span>
                    )}
                  </div>
                  {repo.description && (
                    <p className="text-reported text-ink-3 truncate mt-0.5">{repo.description}</p>
                  )}
                </div>
              </button>
            )
          })}

        {/* The list pages now, so there is a "there is more" affordance rather
            than a walk that pulls the whole account into memory. Selections
            made on an earlier page survive it — the checked set is keyed by
            slug, not by row position. */}
        {nextToken && !error && (
          <div className="flex justify-center py-3">
            <button
              type="button"
              onClick={loadMore}
              disabled={loading}
              className="flex items-center gap-1.5 text-[12px] font-medium text-warm hover:text-warm/80 disabled:opacity-40 transition-colors"
            >
              {loading && <RotateCw size={12} className="animate-spin" />}
              {loading ? 'Loading…' : `Show more (${total - repos.length} left)`}
            </button>
          </div>
        )}
      </div>

      {/* Footer — omitted when the host (the setup wizard) drives its own
          navigation + persistence and just reads the selection via
          onSelectionChange. A standing selected-count keeps the host honest. */}
      {!hideFooter && (
        <div
          className={`flex items-center justify-between border-t border-line-1 py-4 ${
            inline ? 'mt-2' : 'px-6'
          }`}
        >
          <span className="text-ui text-ink-3">
            {checked.size} repo{checked.size !== 1 ? 's' : ''} selected
          </span>
          <div className="flex gap-3">
            {inline && onBack && (
              <button
                type="button"
                onClick={onBack}
                className="text-body text-ink-2 hover:text-ink-1 bg-raised hover:bg-raised border border-line-1 rounded-xl px-4 py-2 transition-colors"
              >
                Back
              </button>
            )}
            {!inline && (
              <button
                type="button"
                onClick={onClose}
                className="text-body text-ink-2 hover:text-ink-1 border border-line-1 rounded-xl px-4 py-2 transition-colors"
              >
                Cancel
              </button>
            )}
            <button
              type="button"
              onClick={() => onSave(Array.from(checked))}
              disabled={checked.size === 0 || saving}
              className="flex items-center gap-1.5 bg-warm hover:bg-warm/90 disabled:opacity-40 text-warm-ink font-medium rounded-xl px-5 py-2 text-body transition-colors"
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
      className="fixed inset-0 z-50 flex items-center justify-center bg-scrim backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="w-full max-w-xl backdrop-blur-xl bg-raised border border-line-1 rounded-2xl shadow-float shadow-black/[0.06] overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {content}
      </div>
    </div>,
    document.body,
  )
}
