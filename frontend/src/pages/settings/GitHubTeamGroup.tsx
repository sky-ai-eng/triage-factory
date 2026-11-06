import { useCallback, useEffect, useRef, useState } from 'react'
import { Section } from './primitives'
import GitHubTeamChecklist from '../../components/GitHubTeamChecklist'
import { httpErrorMessage } from '../../lib/apiClient'
import {
  groupKey,
  identifyGroups,
  listGitHubTeams,
  teamRef,
  type GitHubTeamOption,
  type GitHubTeamRef,
} from '../../lib/githubTeams'
import { fetchTeamGitHubGroups, type GitHubGroup } from './teamConfig'

/** How many teams one read holds. The checklist is a scrolled list, so a
 *  typical org arrives whole and a large one is reached by the search box or
 *  by asking for the next page. */
const TEAM_PAGE_SIZE = 200

/** How long after the last keystroke before asking for the searched list. */
const SEARCH_DEBOUNCE_MS = 250

/** The pages held, and the search they answer. */
interface Held {
  search: string
  items: GitHubTeamOption[]
  next: string
  total: number
}

/**
 * GitHubTeamGroup is the team-scope GitHub-team → TF-team mapping field
 * group: which GitHub teams route their CODEOWNERS review requests to this
 * Triage Factory team. A controlled component — the container owns the
 * selected mappings (`value`) and the PUT /api/teams/{id}/github-
 * groups — so the same group serves the team Settings tab and the setup
 * wizard's team steps.
 *
 * It reads the GitHub teams the team can map from their list route, which
 * searches and orders them, and renders the shared GitHubTeamChecklist body
 * over the pages it holds. On first load it seeds the selection up via
 * onLoaded: a prior saved set wins, with any mapping the server has not
 * identified by GitHub's id yet matched to its team among the rows read
 * (identifyGroups) so a save can name it; otherwise, when includeMembership
 * is set (the onboarding default), the acting admin's own teams start checked,
 * read with the list's `only_mine` filter.
 */
export default function GitHubTeamGroup({
  value,
  onChange,
  teamId,
  canEdit = true,
  includeMembership = false,
  onLoaded,
  refreshSignal = 0,
  bare = false,
}: {
  value: GitHubGroup[]
  onChange: (groups: GitHubGroup[]) => void
  teamId: string
  canEdit?: boolean
  // When true, ask the list to mark the admin's own teams and (absent a saved
  // set) pre-check them. The Settings editor leaves it false — it shows the
  // saved set, no personal pre-check.
  includeMembership?: boolean
  // Fired once after the first successful load with the seed selection, so
  // the container can seed its form state (and a change-detection baseline).
  onLoaded?: (groups: GitHubGroup[]) => void
  // Bumped by the container to re-read the list — e.g. after a Repositories
  // save changed the tracked-owner set — WITHOUT re-seeding the selection.
  // seededRef stays true across the re-read, so onLoaded never fires again
  // and the user's unsaved mapping edits are preserved (a repo save must not
  // flush in-progress GitHub-team edits in a sibling section).
  refreshSignal?: number
  // The setup wizard composes this flush (no Section card, glass search +
  // filter); Settings keeps the carded default.
  bare?: boolean
}) {
  const [held, setHeld] = useState<Held | null>(null)
  const [credentialsMissing, setCredentialsMissing] = useState(false)
  const [search, setSearch] = useState('')
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [searchError, setSearchError] = useState('')
  const seededRef = useRef(false)
  // onLoaded is held in a ref so `load` doesn't depend on it: callers pass an
  // inline closure (a new identity each render), and putting it in load's
  // deps would re-fire the fetch effect every render — an infinite loop.
  const onLoadedRef = useRef(onLoaded)
  useEffect(() => {
    onLoadedRef.current = onLoaded
  }, [onLoaded])

  // Which read the held pages belong to. Searches are typed faster than they
  // resolve, so reads come back out of order; only the latest may land.
  const readSeq = useRef(0)
  // The search the reader is asking for now, for the reads that fire on their
  // own schedule (a retry, a refresh): re-asking an older search would land
  // over the newer one.
  const wanted = useRef('')

  // readPage reads one page under `q` and holds it: a first page replaces
  // what is held, a further one appends. It resolves to null when a later
  // read superseded it.
  const readPage = useCallback(
    async (q: string, pageToken = '') => {
      const seq = ++readSeq.current
      const page = await listGitHubTeams(teamId, {
        search: q,
        includeMembership,
        pageSize: TEAM_PAGE_SIZE,
        pageToken,
      })
      if (seq !== readSeq.current) return null
      setHeld((prev) =>
        pageToken && prev && prev.search === q
          ? {
              ...prev,
              items: [...prev.items, ...page.items],
              next: page.next_page_token,
              total: page.total_count,
            }
          : { search: q, items: page.items, next: page.next_page_token, total: page.total_count },
      )
      setCredentialsMissing(page.credentials_missing)
      setSearchError('')
      return page
    },
    [teamId, includeMembership],
  )

  const load = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const seeding = !seededRef.current
      const [page, saved] = await Promise.all([
        readPage(wanted.current),
        seeding ? fetchTeamGitHubGroups(teamId) : Promise.resolve(null),
      ])
      if (seeding) {
        // Without the saved set there is no seed, and a save must then leave
        // the stored mappings alone rather than replace them with nothing.
        if (saved === null) throw new Error('saved mappings unreadable')
        // Seed once: the saved set is an explicit prior choice and wins;
        // otherwise pre-check the admin's own teams when membership was
        // asked for (onboarding), else nothing.
        const seed =
          saved.length > 0
            ? identifyGroups(saved, page?.items ?? [])
            : includeMembership
              ? (
                  await listGitHubTeams(teamId, {
                    onlyMine: true,
                    includeMembership: true,
                    pageSize: TEAM_PAGE_SIZE,
                  })
                ).items.map(teamRef)
              : []
        seededRef.current = true
        onLoadedRef.current?.(seed)
      }
    } catch (e) {
      setLoadError(httpErrorMessage(e, 'Could not load GitHub teams.'))
    } finally {
      setLoading(false)
    }
  }, [readPage, teamId, includeMembership])

  useEffect(() => {
    void load()
  }, [load])

  // Re-read when the container bumps refreshSignal, keeping the current
  // selection: load() re-runs with seededRef already true, so its onLoaded
  // seed is skipped and `value` (the unsaved selection) is untouched. The
  // leading guard skips the mount run, which the effect above already covers.
  const refreshedOnce = useRef(false)
  useEffect(() => {
    if (!refreshedOnce.current) {
      refreshedOnce.current = true
      return
    }
    void load()
  }, [refreshSignal, load])

  // The first page for the search as it settles. The mount read is load()'s,
  // so this answers changes only.
  const searchedOnce = useRef(false)
  useEffect(() => {
    const q = search.trim()
    wanted.current = q
    if (!searchedOnce.current) {
      searchedOnce.current = true
      return
    }
    const id = setTimeout(
      () =>
        void readPage(q).catch((e) =>
          setSearchError(httpErrorMessage(e, 'Could not search GitHub teams.')),
        ),
      q ? SEARCH_DEBOUNCE_MS : 0,
    )
    return () => clearTimeout(id)
  }, [search, readPage])

  const loadMore = useCallback(() => {
    if (!held?.next) return
    void readPage(held.search, held.next).catch((e) =>
      setSearchError(httpErrorMessage(e, 'Could not load more GitHub teams.')),
    )
  }, [held, readPage])

  const toggle = (team: GitHubTeamRef) => {
    const k = groupKey(team)
    onChange(
      value.some((g) => groupKey(g) === k)
        ? value.filter((g) => groupKey(g) !== k)
        : [...value, teamRef(team)],
    )
  }

  const inner = (
    <>
      <div className={bare ? 'mb-4 space-y-1.5' : 'mb-3'}>
        <h2
          className={
            bare
              ? 'text-[19px] font-medium tracking-tight text-ink-1'
              : 'text-body font-medium text-ink-2'
          }
        >
          GitHub teams
        </h2>
        <p
          className={
            bare
              ? 'text-body leading-relaxed text-ink-3'
              : 'mt-1 text-ui leading-relaxed text-ink-3'
          }
        >
          Assign GitHub teams to this Triage Factory team. When a pull request requests review from
          one of these GitHub teams (via CODEOWNERS), the work routes to this team&rsquo;s board.
          Teams are matched by GitHub&rsquo;s id, so renaming one keeps its mapping. No membership
          or nesting is read.
        </p>
      </div>

      <GitHubTeamChecklist
        rows={held?.items ?? []}
        total={held?.total ?? 0}
        searched={!!held?.search}
        hasMore={!!held?.next}
        onLoadMore={loadMore}
        search={search}
        onSearchChange={setSearch}
        listError={searchError}
        selected={value}
        onToggle={toggle}
        loading={loading}
        error={loadError}
        onRetry={() => void load()}
        disabled={!canEdit}
        bare={bare}
        emptyLabel={
          credentialsMissing
            ? 'No GitHub credential connected to this organization reaches the owners of this team’s repositories. Reconnect GitHub, then reopen this panel.'
            : 'No GitHub teams found. Connect GitHub and configure repositories whose org exposes teams, then reopen this panel.'
        }
      />

      {/* Mirror ReposGroup's reassurance: a failed load leaves the selection
          unseeded (undefined), so a save skips it rather than clearing the
          stored mappings. Say so, since the checklist's error alone doesn't. */}
      {loadError && (
        <p className="mt-2 text-ui italic text-warm">
          Couldn&rsquo;t load GitHub teams — existing mappings will be left unchanged. Retry above
          to edit them.
        </p>
      )}
    </>
  )

  return bare ? inner : <Section>{inner}</Section>
}
