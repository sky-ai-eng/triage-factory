import { useState } from 'react'
import { Section, Field, inputClass } from './primitives'
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
 */
export default function JiraAccessGroup({
  value,
  onChange,
  connected,
  onConnected,
  onDisconnected,
}: {
  value: JiraAccessValue
  onChange: (patch: Partial<JiraAccessValue>) => void
  connected: boolean
  onConnected?: (url: string) => void
  onDisconnected?: () => void
}) {
  const [connecting, setConnecting] = useState(false)
  const [connectError, setConnectError] = useState<string | null>(null)

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

  return (
    <Section>
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-[13px] font-medium text-text-secondary">Jira connection</h2>
        {connected && (
          <button
            type="button"
            onClick={disconnect}
            className="text-[11px] text-dismiss hover:text-dismiss/80 transition-colors"
          >
            Disconnect
          </button>
        )}
      </div>

      {!connected ? (
        /* Stage 1: Connect credentials */
        <div className="space-y-3">
          <Field label="Base URL">
            <input
              type="url"
              placeholder="https://jira.yourcompany.com"
              value={value.jira_url}
              onChange={(e) => onChange({ jira_url: e.target.value })}
              className={inputClass}
            />
          </Field>
          <Field label="Personal Access Token">
            <input
              type="password"
              placeholder="Jira Personal Access Token"
              value={value.jira_pat}
              onChange={(e) => onChange({ jira_pat: e.target.value })}
              className={inputClass}
            />
          </Field>
          {connectError && (
            <div className="rounded-xl px-4 py-2.5 text-[13px] bg-dismiss/[0.08] border border-dismiss/20 text-dismiss">
              {connectError}
            </div>
          )}
          <button
            type="button"
            onClick={connect}
            disabled={connecting || !value.jira_url.trim() || !value.jira_pat.trim()}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {connecting ? 'Connecting...' : 'Connect'}
          </button>
        </div>
      ) : (
        /* Stage 2: connected — status only (poll interval lives in the
           shared poller-timing group). */
        <div className="flex items-center gap-2 rounded-xl bg-claim/[0.06] border border-claim/15 px-4 py-2.5">
          <div className="w-1.5 h-1.5 rounded-full bg-claim shrink-0" />
          <span className="text-[12px] text-claim">
            Connected to {value.jira_url.replace(/^https?:\/\//, '')}
          </span>
        </div>
      )}
    </Section>
  )
}
