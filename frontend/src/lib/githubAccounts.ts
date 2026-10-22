import { apiJSON, HttpError, httpErrorMessage } from './apiClient'

/** A GitHub account as the org-scoped account reads answer it: GitHub's id,
 *  which a GitHub predicate's identity filters (author_id_in and its
 *  siblings) store, and the login it is called now. */
export interface GitHubAccount {
  github_user_id: string
  login: string
}

/** The answer to looking a typed login up: its account, or a sentence saying
 *  why there is none to store. */
export type GitHubAccountLookup =
  | { ok: true; account: GitHubAccount }
  | { ok: false; error: string }

const accountsPath = (orgId: string) => `/api/orgs/${encodeURIComponent(orgId)}/github/users`

/** findGitHubAccountByLogin resolves a login (a bot as "<slug>[bot]") to its
 *  account through the org's GitHub credential. A login no account holds is
 *  an answer, not a failure: the caller shows it beside the field. */
export async function findGitHubAccountByLogin(
  orgId: string,
  login: string,
): Promise<GitHubAccountLookup> {
  try {
    const account = await apiJSON<GitHubAccount>(
      `${accountsPath(orgId)}/by-name/${encodeURIComponent(login)}`,
    )
    return { ok: true, account }
  } catch (err) {
    if (err instanceof HttpError && err.status === 404) {
      return { ok: false, error: `No GitHub account is called ${login}.` }
    }
    return { ok: false, error: httpErrorMessage(err, `Could not look ${login} up on GitHub.`) }
  }
}

/** getGitHubAccount reads the account an id names, under its current login,
 *  or null when it cannot be read — the caller then shows the id itself. */
export async function getGitHubAccount(orgId: string, id: string): Promise<GitHubAccount | null> {
  return apiJSON<GitHubAccount>(`${accountsPath(orgId)}/${encodeURIComponent(id)}`).catch(
    () => null,
  )
}
