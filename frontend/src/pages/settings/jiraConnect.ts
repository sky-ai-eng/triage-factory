// The Jira connect action, factored out so every surface drives it the same
// way the GitHub PAT flow drives its save: the caller's Continue (setup
// wizard) / Save (Settings) performs the connect, rather than a field group
// owning a separate Connect button. POST /api/jira/connect validates the
// url+PAT pair server-side (reachability + auth) and persists the credential,
// so a successful result IS the validation — there's no separate probe.
//
// The disconnect lifecycle (DELETE /api/integrations/jira + the URL-column
// clear) stays inside JiraAccessGroup: it's an inline action on the
// already-connected state, with no Continue/Save to fold into.

// connectJira posts the org-level Jira credential pair. Returns a
// discriminated result mirroring saveOrgConfig — the caller surfaces the
// error inline (wizard error line) or as a toast (Settings).
export async function connectJira(
  url: string,
  pat: string,
): Promise<{ ok: true } | { ok: false; error: string }> {
  try {
    const res = await fetch('/api/jira/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: url.trim(), pat: pat.trim() }),
    })
    const body = await res.json().catch(() => ({}))
    if (!res.ok) {
      return { ok: false, error: body.error || 'Connection failed' }
    }
    return { ok: true }
  } catch {
    return { ok: false, error: 'Could not connect to server' }
  }
}
