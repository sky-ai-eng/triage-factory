import { apiFetch, apiJSON } from './apiClient'

/** One option in the repository picker. These are the fields the backend
 *  mirrors for the picker specifically — not a passthrough of GitHub's
 *  repository object, which is what the old proxy list leaked. */
export interface GitHubRepo {
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
 *  workspace yet and a refresh has been kicked. It matters because the
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
 *  workspace arrives whole, small enough that a 3,000-repo account does not
 *  land in the DOM at once — the search box below is the affordance for
 *  reaching past it, and it filters server-side. */
export const REPO_PAGE_SIZE = 100

/** listReachableRepos reads one page of the repositories this workspace's
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

/** refreshReachableRepos asks the server to re-read the workspace's reachable
 *  set from GitHub now, past the staleness window it normally waits out.
 *
 *  It returns as soon as the refresh is accepted — the work happens out of band
 *  and the result arrives as a `github_repos_updated` websocket ping. This is
 *  the affordance for "I just granted the App another repository and it isn't
 *  in the list": without it, the answer would be "wait". */
export async function refreshReachableRepos(): Promise<void> {
  await apiFetch('/api/github/repos/refresh', { method: 'POST' })
}
