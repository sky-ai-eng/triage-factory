import { apiList } from './apiClient'

/** One option in the Jira project picker: the key rules and JQL are written
 *  against, plus the name a human recognizes it by. These are the only two
 *  fields the route serves — a project object from either Jira deployment also
 *  carries avatars, a lead and a category, none of which this renders. */
export interface JiraProjectCandidate {
  key: string
  name: string
}

/** How many candidates one page carries. A type-to-filter box is the affordance
 *  for reaching past it, and the filter runs server-side, so this is sized to
 *  hold a typical instance's whole catalog rather than to be scrolled through.
 *  Jira Cloud caps its own catalog page below this and simply serves fewer. */
export const JIRA_PROJECT_PAGE_SIZE = 100

export interface JiraProjectPage {
  items: JiraProjectCandidate[]
  /** True when the server had rows this page didn't carry. There is no count to
   *  show beside it: the catalog is proxied live from Jira, which reports no
   *  total, so `total_count` is null on this list by contract. It exists to tell
   *  the reader that narrowing the search will reveal more, which is the only
   *  thing a truncated page can honestly say. */
  hasMore: boolean
}

/** listJiraProjects reads one page of the projects this workspace's Jira
 *  credential can see.
 *
 *  `q` is matched server-side against the project key and name. Nothing is
 *  cached or mirrored: the catalog is fetched from Jira on each read, which is
 *  what a list of dozens behind a single org credential is worth. */
export async function listJiraProjects(
  q: string,
  options: { signal?: AbortSignal } = {},
): Promise<JiraProjectPage> {
  const page = await apiList<JiraProjectCandidate>(
    '/api/jira/projects/list',
    { q, page_size: JIRA_PROJECT_PAGE_SIZE },
    options,
  )
  return { items: page.items, hasMore: page.next_page_token !== '' }
}
