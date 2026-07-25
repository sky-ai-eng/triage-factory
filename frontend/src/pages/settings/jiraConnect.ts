// The Jira connect action, factored out so every surface drives it the same
// way the GitHub PAT flow drives its save: the caller's Continue (setup
// wizard) / Save (Settings) performs the connect, rather than a field group
// owning a separate Connect button. PUT
// /api/orgs/{org}/jira-access/credential validates the credential server-side
// (reachability + auth) and persists it, so a successful result IS the
// validation — there's no separate probe.
//
// The unbind is the DELETE on the same resource (disconnectJira in
// orgCredentials.ts, alongside the GitHub pair), driven from JiraAccessGroup:
// it's an inline action on the already-connected state, with no Continue/Save
// to fold into.

// JiraDeployment is the backend a Jira org connects to. It mirrors the
// jira.Deployment enum (Go side) and is the explicit choice made in the
// onboarding deployment picker, NOT inferred from the field group: Cloud
// authenticates with an Atlassian API token (email + token, Basic / REST v3),
// Data Center with a personal access token (Bearer / REST v2).
export type JiraDeployment = 'cloud' | 'data_center'

// JIRA_DEPLOYMENT_OPTIONS is the shared label set for the deployment picker,
// so the wizard's mode step and the Settings deployment toggle present the same
// two choices without drift. Callers feed it to ChoiceCards (the wizard adds a
// per-card status hint; Settings uses it as-is).
export const JIRA_DEPLOYMENT_OPTIONS: { kind: JiraDeployment; title: string; detail: string }[] = [
  {
    kind: 'cloud',
    title: 'Atlassian Cloud',
    detail: 'Hosted by Atlassian at *.atlassian.net. Connects with an account email + API token.',
  },
  {
    kind: 'data_center',
    title: 'Data Center / Server',
    detail: 'Self-hosted on your own domain. Connects with a personal access token.',
  },
]

// isJiraCloudHost classifies a Jira base URL (or bare host) as Atlassian Cloud
// by hostname shape — every Cloud site is served from "<site>.atlassian.net".
// It mirrors the Go jira.DeploymentForHost primitive so the frontend's
// smart-default (pre-selecting the deployment picker) agrees with the backend.
// The deployment the credential is stored under is the user's explicit pick,
// not this guess; this only seeds the default and labels a returning org.
export function isJiraCloudHost(url: string): boolean {
  const raw = url.trim().toLowerCase()
  if (raw === '') return false
  let host = raw
  try {
    const parsed = new URL(raw)
    if (parsed.hostname) host = parsed.hostname
  } catch {
    // Bare host without a scheme — strip any scheme remnant, path, and port.
    host = raw
      .replace(/^[a-z][a-z0-9+.-]*:\/\//, '')
      .split('/')[0]
      .split(':')[0]
  }
  return host === 'atlassian.net' || host.endsWith('.atlassian.net')
}

// JiraConnectCreds carries both credential shapes; connectJira sends only the
// pair the chosen deployment needs.
export interface JiraConnectCreds {
  jira_pat: string
  jira_email: string
  jira_api_token: string
}

// connectJira binds the org-level Jira service credential. The deployment
// (chosen upstream) selects which credential shape is sent — Cloud sends
// {url, email, token}; Data Center sends {url, pat}. Returns a discriminated
// result mirroring saveOrgConfig — the caller surfaces the error inline
// (wizard error line) or as a toast (Settings).
export async function connectJira(
  orgId: string,
  url: string,
  deployment: JiraDeployment,
  creds: JiraConnectCreds,
): Promise<{ ok: true } | { ok: false; error: string }> {
  const body =
    deployment === 'cloud'
      ? { url: url.trim(), email: creds.jira_email.trim(), token: creds.jira_api_token.trim() }
      : { url: url.trim(), pat: creds.jira_pat.trim() }
  try {
    const res = await fetch(`/api/orgs/${orgId}/jira-access/credential`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    const resBody = await res.json().catch(() => ({}))
    if (!res.ok) {
      return { ok: false, error: resBody.error || 'Connection failed' }
    }
    return { ok: true }
  } catch {
    return { ok: false, error: 'Could not connect to server' }
  }
}
