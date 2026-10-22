import { apiJSON, apiList, HttpError, httpErrorMessage } from './apiClient'

/** A person in the org's Linear workspace: Linear's user id, which a Linear
 *  predicate's people filters (assignee_in, creator_in, commenter_in) store,
 *  the full name, and the display name, which Linear keeps unique within a
 *  workspace. app is true for an integration's app user rather than a person:
 *  one can create issues and comment, but it is not who an issue is assigned
 *  to. */
export interface LinearUser {
  id: string
  name: string
  display_name: string
  active: boolean
  app: boolean
}

/** How many people one search offers to pick from. */
const LINEAR_USER_SEARCH_SIZE = 10

/** A search's answer: the users it matched, or a sentence saying why there
 *  are none to offer. */
export type LinearUserSearch = { ok: true; users: LinearUser[] } | { ok: false; error: string }

/** A read by id's answer: the user, or a sentence saying why there is none. */
export type LinearUserLookup = { ok: true; user: LinearUser } | { ok: false; error: string }

const usersPath = (orgId: string) => `/api/orgs/${encodeURIComponent(orgId)}/linear/users`

/** searchLinearUsers lists the workspace's active users whose name, display
 *  name or email contains q. An empty list is an answer, not a failure. */
export async function searchLinearUsers(orgId: string, q: string): Promise<LinearUserSearch> {
  try {
    const page = await apiList<LinearUser>(`${usersPath(orgId)}/list`, {
      q,
      page_size: LINEAR_USER_SEARCH_SIZE,
    })
    return { ok: true, users: page.items }
  } catch (err) {
    return { ok: false, error: httpErrorMessage(err, `Could not search Linear for ${q}.`) }
  }
}

/** findLinearUser reads one user of the workspace by id. An id no user holds
 *  is an answer, not a failure: the caller shows it beside the field. */
export async function findLinearUser(orgId: string, id: string): Promise<LinearUserLookup> {
  try {
    const user = await apiJSON<LinearUser>(`${usersPath(orgId)}/${encodeURIComponent(id)}`)
    return { ok: true, user }
  } catch (err) {
    if (err instanceof HttpError && err.status === 404) {
      return { ok: false, error: `No Linear user has the id ${id}.` }
    }
    return { ok: false, error: httpErrorMessage(err, `Could not look ${id} up in Linear.`) }
  }
}

/** linearUserHandle is what a person is shown by: the display name, which is
 *  unique in the workspace, else the full name. */
export const linearUserHandle = (u: LinearUser) => u.display_name || u.name

/** isLinearID reports whether s is a Linear id as Linear writes one: a UUID in
 *  lowercase. */
export const isLinearID = (s: string) =>
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(s)
