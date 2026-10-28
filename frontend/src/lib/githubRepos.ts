import { apiFetch, apiJSON } from './apiClient'

/** One option in the repository picker. These are the fields the backend
 *  mirrors for the picker specifically — not a passthrough of GitHub's
 *  repository object, which is what the old proxy list leaked. */
export interface GitHubRepo {
  /** The registry row TF holds for the repository, '' when it holds none. The
   *  server matches it by GitHub id, and by name only when either side has no
   *  id, so an option and a tracked entry with the same name and different
   *  ids carry different rows. */
  repository_id: string
  /** GitHub's own id for the repository: what an option with no registry row
   *  is registered by (`registerRepository`). A rename moves `full_name`,
   *  never this. */
  github_id: string
  full_name: string
  html_url: string
  description: string
  language: string
  pushed_at: string
  private: boolean
}

/** Whether the page below is the mirror's answer or a placeholder for one.
 *
 *  `discovering` is the first-ever open: nothing has been mirrored for this
 *  org yet and a refresh has been kicked. It matters because the
 *  alternative — an empty `items` — reads as "you have no repositories", which
 *  is a different and alarming claim. */
export type RepoListStatus = 'ready' | 'discovering'

/** The picker list envelope: the standard paging keys plus the readiness
 *  discriminator. Every key is present in both states, so a caller renders off
 *  `status` rather than inferring from an empty array.
 *
 *  `total_count` is a real number on both credential classes (PAT and GitHub
 *  App alike) — the list is served from a local mirror, not proxied — so
 *  "N of M" is renderable here, unlike on a proxy list. */
export interface RepoListPage {
  items: GitHubRepo[]
  next_page_token: string
  total_count: number
  status: RepoListStatus
}

/** How many options one page of the picker holds. Large enough that a typical
 *  org arrives whole, small enough that a 3,000-repo account does not
 *  land in the DOM at once — the search box below is the affordance for
 *  reaching past it, and it filters server-side. */
export const REPO_PAGE_SIZE = 100

/** listReachableRepos reads one page of the repositories this org's
 *  GitHub credentials can reach.
 *
 *  `q` is matched server-side against the slug, the description and the
 *  language, which is what lets this surface stop holding the whole set in
 *  memory to search it. `pageToken` continues a previous page and is only ever
 *  the token the server minted — it is valid solely for the `q` it was issued
 *  under, so it must not cross a search change. */
export async function listReachableRepos(q: string, pageToken = ''): Promise<RepoListPage> {
  const page = await apiJSON<Partial<RepoListPage>>('/api/github/repos/list', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      q,
      page_size: REPO_PAGE_SIZE,
      ...(pageToken ? { page_token: pageToken } : {}),
    }),
  })
  return {
    items: page.items ?? [],
    next_page_token: page.next_page_token ?? '',
    total_count: page.total_count ?? 0,
    status: page.status === 'discovering' ? 'discovering' : 'ready',
  }
}

/** refreshReachableRepos asks the server to re-read the org's reachable
 *  set from GitHub now, past the staleness window it normally waits out.
 *
 *  It returns as soon as the refresh is accepted — the work happens out of band
 *  and the result arrives as a `github_repos_updated` websocket ping. This is
 *  the affordance for "I just granted the App another repository and it isn't
 *  in the list": without it, the answer would be "wait". */
export async function refreshReachableRepos(): Promise<void> {
  await apiFetch('/api/github/repos/refresh', { method: 'POST' })
}

/** A repository as a team's tracked set names it. GET and PUT
 *  /api/teams/{id}/github-repos answer in this shape: `id` is the registry row
 *  id the PUT takes, `github_id` is GitHub's id ('' while TF has not learned
 *  it), `owner`/`repo` are the current name, for display, and `retired_at` is
 *  set when GitHub no longer serves the repository to the org and has
 *  given its name to another one. A retired row stays in the set until a save
 *  drops it, and nothing polls it; the repository now under the name is a
 *  different candidate, with an id of its own.
 *
 *  A picker holds its selection in the same shape, which is why `id` may be
 *  '': a candidate picked from the reachable list that has no registry row
 *  gets one when `saveTeamRepos` registers it by `github_id`. */
export interface RepoRef {
  id: string
  github_id: string
  owner: string
  repo: string
  retired_at?: string | null
}

/** Whether a tracked ref names a repository GitHub no longer serves. */
export const isRetired = (r: RepoRef): boolean => !!r.retired_at

export const repoRefSlug = (r: RepoRef): string => `${r.owner}/${r.repo}`

/** The keys a ref answers to: its registry id and its GitHub id, whichever it
 *  carries, and its folded name only when it carries neither. Two refs name one
 *  repository when they share a key, so a name matches only between two refs
 *  that have no id at all; a name shared with a ref that has one is not a
 *  match, because GitHub may have given that name to another repository. */
export const repoRefKeys = (r: RepoRef): string[] => {
  const keys = [...(r.id ? [`id:${r.id}`] : []), ...(r.github_id ? [`gh:${r.github_id}`] : [])]
  return keys.length > 0 ? keys : [`name:${repoRefSlug(r).toLowerCase()}`]
}

/** The selection key for a ref: the first of its `repoRefKeys`. A selection is
 *  held under it; what a save sends is the registry id. */
export const repoRefKey = (r: RepoRef): string => repoRefKeys(r)[0]

/** Whether two refs name one repository. */
export const sameRepoRef = (a: RepoRef, b: RepoRef): boolean => {
  const keys = new Set(repoRefKeys(a))
  return repoRefKeys(b).some((k) => keys.has(k))
}

/** The keys a candidate answers to: those of its selection entry. */
export const candidateKeys = (c: GitHubRepo): string[] => repoRefKeys(refFromCandidate(c))

/** refFromCandidate is the selection entry for a candidate: its registry row
 *  when TF holds one, else '' until a save registers it. */
export function refFromCandidate(c: GitHubRepo): RepoRef {
  const slash = c.full_name.indexOf('/')
  return {
    id: c.repository_id ?? '',
    github_id: c.github_id,
    owner: c.full_name.slice(0, slash),
    repo: c.full_name.slice(slash + 1),
  }
}

/** registerRepository registers a candidate by its GitHub id (POST /api/repos)
 *  and returns its registry row id — the row TF already holds for it, or a new
 *  one. Refused (422) when the org's credentials cannot reach it. */
export async function registerRepository(githubId: string): Promise<string> {
  const row = await apiJSON<{ id: string }>('/api/repos', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ github_id: githubId }),
  })
  return row.id
}
