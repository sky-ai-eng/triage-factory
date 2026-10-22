import { apiList, apiListAll } from './apiClient'
import type { JiraStatusRef } from '../components/JiraStatusRule'

/** One option in the Jira project picker: Jira's id for the project, which a
 *  save names a project being added by and the project routes address it by,
 *  plus the key and name a human recognizes it by. These are the only fields
 *  the route serves — a project object from either Jira deployment also
 *  carries avatars, a lead and a category, none of which this renders. */
export interface JiraProjectCandidate {
  jira_id: string
  key: string
  name: string
}

/** The selection key for a project: Jira's id when it has one, its key
 *  otherwise — a tracked project the poll has not resolved against Jira yet.
 *  It keys per-row view state and matches tracked projects against catalog
 *  candidates; what a save sends is the registry id of a tracked project and
 *  the Jira id of one being added. */
export const jiraProjectRef = (p: { jira_id?: string; key: string }): string =>
  p.jira_id ? `jira:${p.jira_id}` : `key:${p.key.trim().toUpperCase()}`

/** The selection keys a candidate answers to: its Jira id, and its key for a
 *  tracked project that carries no Jira id. */
export const candidateRefs = (c: JiraProjectCandidate): string[] => [
  `jira:${c.jira_id}`,
  `key:${c.key.trim().toUpperCase()}`,
]

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

/** listJiraProjects reads one page of the projects the org's Jira credential
 *  can see.
 *
 *  `q` is matched server-side against the project key and name. Nothing is
 *  cached or mirrored: the catalog is fetched from Jira on each read, which is
 *  what a list of dozens behind a single org credential is worth. */
export async function listJiraProjects(
  orgId: string,
  q: string,
  options: { signal?: AbortSignal } = {},
): Promise<JiraProjectPage> {
  const page = await apiList<JiraProjectCandidate>(
    `/api/orgs/${encodeURIComponent(orgId)}/jira/projects/list`,
    { q, page_size: JIRA_PROJECT_PAGE_SIZE },
    options,
  )
  return { items: page.items, hasMore: page.next_page_token !== '' }
}

/** listJiraStatuses reads one project's whole workflow, ordered by name, for
 *  the project Jira's id `jiraId` names. A project's workflow is a handful of
 *  statuses and the rules board needs all of them, so the list is walked to
 *  the end. */
export async function listJiraStatuses(
  orgId: string,
  jiraId: string,
  options: { signal?: AbortSignal } = {},
): Promise<JiraStatusRef[]> {
  return apiListAll<JiraStatusRef>(
    `/api/orgs/${encodeURIComponent(orgId)}/jira/projects/${encodeURIComponent(jiraId)}/statuses/list`,
    {},
    options,
  )
}

/** One of the Jira instance's priorities, or one issue type of a project: the
 *  id a predicate names it by and the name it is shown as. */
export interface JiraCatalogValue {
  id: string
  name: string
}

/** listJiraPriorities reads the Jira instance's priorities, in Jira's order
 *  (highest first). The list is short and a picker offers all of it. */
export async function listJiraPriorities(
  orgId: string,
  options: { signal?: AbortSignal } = {},
): Promise<JiraCatalogValue[]> {
  return apiListAll<JiraCatalogValue>(
    `/api/orgs/${encodeURIComponent(orgId)}/jira/priorities/list`,
    {},
    options,
  )
}

/** listJiraIssueTypes reads the issue types of the project Jira's id `jiraId`
 *  names, ordered by name. */
export async function listJiraIssueTypes(
  orgId: string,
  jiraId: string,
  options: { signal?: AbortSignal } = {},
): Promise<JiraCatalogValue[]> {
  return apiListAll<JiraCatalogValue>(
    `/api/orgs/${encodeURIComponent(orgId)}/jira/projects/${encodeURIComponent(jiraId)}/issue-types/list`,
    {},
    options,
  )
}
