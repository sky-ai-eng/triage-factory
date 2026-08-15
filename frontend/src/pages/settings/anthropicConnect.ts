// The Anthropic-key connect action, factored out the same way connectJira is
// (jiraConnect.ts): every surface — the setup wizard's Continue and the
// Settings section's Save — drives this one helper, so the key is captured
// through a single validated path. POST /api/anthropic/connect validates the
// key server-side (GET https://api.anthropic.com/v1/models) and persists it on
// success, so a successful result IS the validation — there's no separate
// probe. An empty key is the "use system Claude Code credentials" selection
// (local only): the backend clears any stored key so the resolver falls back to
// the subscription/env path.
//
// Returns a discriminated result mirroring connectJira / saveOrgConfig — the
// caller surfaces the error inline (wizard error line) or as a toast (Settings).

// CLAUDE_SOURCE_OPTIONS is the shared label set for the "system creds vs BYOK"
// picker, so the wizard's source step and the Settings source toggle present
// the same two choices without drift — the Claude analog of
// JIRA_DEPLOYMENT_OPTIONS. Lives here (a non-component module) rather than in
// ClaudeStep.tsx so the component file only exports components.
import { apiFetch, httpErrorMessage } from '../../lib/apiClient'

export const CLAUDE_SOURCE_OPTIONS: { kind: 'system' | 'byok'; title: string; detail: string }[] = [
  {
    kind: 'system',
    title: 'Use system Claude Code credentials',
    detail: 'Run under the Claude Code subscription or ANTHROPIC_API_KEY already on this machine.',
  },
  {
    kind: 'byok',
    title: 'Bring your own API key',
    detail: 'Paste an Anthropic API key for Triage Factory to use for delegated runs.',
  },
]

export async function connectAnthropic(
  apiKey: string,
): Promise<{ ok: true } | { ok: false; error: string }> {
  try {
    await apiFetch('/api/anthropic/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ api_key: apiKey }),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not validate the API key.') }
  }
}
