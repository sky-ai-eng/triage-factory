import { useState } from 'react'
import { Section, Field, inputClass, glassInputClass } from './primitives'
import { toast } from '../../components/Toast/toastStore'
import { readError } from '../../lib/api'

interface JiraAccessValue {
  jira_url: string
  jira_pat: string
}

/**
 * JiraAccessGroup is the org-level Jira credential field group. It owns the
 * two-stage connect/disconnect lifecycle against the stable endpoints
 * (POST /api/jira/connect, DELETE /api/integrations/jira) — the same flow in
 * every surface — while the container owns the url/pat form values and the
 * `connected` flag.
 *
 * Project tracking + status rules are TEAM-level (a separate surface), so
 * they live outside this group. On a successful connect/disconnect the
 * component fires onConnected(url) / onDisconnected so the container can do
 * its own scope-specific follow-up (e.g. wiping team project config when the
 * instance URL changes, or advancing a wizard step).
 *
 * Multi-mode Jira OAuth (3LO/2LO) is unbuilt; until it lands the only
 * connect affordance is this PAT path, which works in both modes.
 *
 * showBaseUrl (default true) mirrors GitHubAccessGroup: the setup wizard
 * confirms the Jira URL in its own reachability sub-step and feeds it in via
 * `value.jira_url`, so it suppresses the field here and renders this group as
 * the PAT-auth sub-step alone — no forked connect logic.
 *
 * bare (default false) drops the carded `<Section>` chrome + heading and uses
 * the flush glass field material, so the setup wizard and the redesigned
 * Settings stack compose it flush like the other groups; Settings' legacy
 * carded look is the default.
 */
export default function JiraAccessGroup({
  value,
  onChange,
  connected,
  onConnected,
  onDisconnected,
  showBaseUrl = true,
  bare = false,
}: {
  value: JiraAccessValue
  onChange: (patch: Partial<JiraAccessValue>) => void
  connected: boolean
  onConnected?: (url: string) => void
  onDisconnected?: () => void
  showBaseUrl?: boolean
  bare?: boolean
}) {
  const [connecting, setConnecting] = useState(false)
  const [connectError, setConnectError] = useState<string | null>(null)
  const field = bare ? glassInputClass : inputClass

  const connect = async () => {
    setConnecting(true)
    setConnectError(null)
    try {
      const res = await fetch('/api/jira/connect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: value.jira_url.trim(), pat: value.jira_pat.trim() }),
      })
      const body = await res.json().catch(() => ({}))
      if (!res.ok) {
        setConnectError(body.error || 'Connection failed')
        return
      }
      // Clear the entered PAT — it's persisted server-side now and we never
      // re-display it.
      onChange({ jira_pat: '' })
      onConnected?.(value.jira_url.trim())
    } catch {
      setConnectError('Could not connect to server')
    } finally {
      setConnecting(false)
    }
  }

  const disconnect = async () => {
    // DELETE /api/integrations/jira clears the SecretStore entries
    // (URL + PAT) but leaves org_settings.jira_base_url populated. Once that
    // succeeds the connection is effectively broken, so we reflect the
    // disconnected state regardless of whether the URL-column clear lands.
    let credCleared = false
    try {
      const credRes = await fetch('/api/integrations/jira', { method: 'DELETE' })
      if (!credRes.ok) {
        toast.error(await readError(credRes, 'Failed to disconnect Jira'))
        return
      }
      credCleared = true
      // Follow with an explicit org POST so the URL column also clears,
      // otherwise reloading would show the stale URL prefilled with
      // has_jira_pat:false. This is a deliberately sparse body: the
      // /api/settings/org handler treats absent fields as nil/unchanged
      // (pointer fields) or empty-omit (interval strings), so the GitHub
      // URL/PAT, poll intervals, and model cap are untouched.
      const orgRes = await fetch('/api/settings/org', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ jira_base_url: '' }),
      })
      if (!orgRes.ok) {
        toast.error(
          await readError(
            orgRes,
            'Jira credentials were removed, but clearing the saved URL failed',
          ),
        )
      }
    } catch {
      toast.error(
        credCleared
          ? 'Jira credentials were removed, but clearing the saved URL failed.'
          : 'Could not reach the server to disconnect Jira.',
      )
      if (!credCleared) return
    }
    onChange({ jira_url: '', jira_pat: '' })
    onDisconnected?.()
  }

  const body = (
    <>
      {/* The carded layout titles itself ("Jira connection") + carries the
          Disconnect on the heading row; bare relies on the host's section
          title and tucks Disconnect above the status line. */}
      {!bare && (
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-[13px] font-medium text-text-secondary">Jira connection</h2>
          {connected && (
            <button
              type="button"
              onClick={disconnect}
              className="text-[11px] text-dismiss transition-colors hover:text-dismiss/80"
            >
              Disconnect
            </button>
          )}
        </div>
      )}

      {!connected ? (
        /* Stage 1: Connect credentials */
        <div className="space-y-3">
          {showBaseUrl && (
            <Field label="Base URL">
              <input
                type="url"
                placeholder="https://jira.yourcompany.com"
                value={value.jira_url}
                onChange={(e) => onChange({ jira_url: e.target.value })}
                className={field}
              />
            </Field>
          )}
          <Field label="Personal Access Token">
            <input
              type="password"
              placeholder="Jira Personal Access Token"
              value={value.jira_pat}
              onChange={(e) => onChange({ jira_pat: e.target.value })}
              className={field}
            />
          </Field>
          {connectError && (
            <div className="rounded-xl border border-dismiss/20 bg-dismiss/[0.08] px-4 py-2.5 text-[13px] text-dismiss">
              {connectError}
            </div>
          )}
          <button
            type="button"
            onClick={connect}
            disabled={connecting || !value.jira_url.trim() || !value.jira_pat.trim()}
            className="w-full rounded-xl bg-accent px-4 py-2.5 text-[13px] font-medium text-white transition-colors hover:bg-accent/90 disabled:opacity-40"
          >
            {connecting ? 'Connecting...' : 'Connect'}
          </button>
        </div>
      ) : (
        /* Stage 2: connected — status + (bare) the Disconnect the carded
           layout puts on its heading row. */
        <div className="space-y-2">
          <div className="flex items-center gap-2 rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-2.5">
            <div className="h-1.5 w-1.5 shrink-0 rounded-full bg-claim" />
            <span className="text-[12px] text-claim">
              Connected to {value.jira_url.replace(/^https?:\/\//, '')}
            </span>
            {bare && (
              <button
                type="button"
                onClick={disconnect}
                className="ml-auto text-[11px] text-dismiss transition-colors hover:text-dismiss/80"
              >
                Disconnect
              </button>
            )}
          </div>
        </div>
      )}
    </>
  )

  return bare ? body : <Section>{body}</Section>
}
