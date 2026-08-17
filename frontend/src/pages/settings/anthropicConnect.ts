// The Anthropic-key actions, factored out the same way connectJira is
// (jiraConnect.ts): every surface — the setup wizard's Continue and the
// Settings section's Save — drives these helpers, so the key is captured
// through a single validated path. PUT /api/orgs/{org}/llm/anthropic validates
// the key server-side (GET https://api.anthropic.com/v1/models) and persists it
// on success, so a successful result IS the validation — there's no separate
// probe.
//
// A blank key is not a spelling of anything. Removing the stored key is the
// DELETE; the route used to read a blank `api_key` as "clear this key and every
// Bedrock secret too", which is why "use system credentials" is now two
// explicit disconnects (disconnectLLM below) rather than one overloaded write.
//
// Returns a discriminated result mirroring connectJira / saveOrgConfig — the
// caller surfaces the error inline (wizard error line) or as a toast (Settings).

// CLAUDE_SOURCE_OPTIONS is the shared label set for the "system creds vs BYOK"
// picker, so the wizard's source step and the Settings source toggle present
// the same two choices without drift — the Claude analog of
// JIRA_DEPLOYMENT_OPTIONS. Lives here (a non-component module) rather than in
// ClaudeStep.tsx so the component file only exports components.
import { apiFetch, httpErrorMessage } from '../../lib/apiClient'
import { disconnectBedrock } from './bedrockConnect'

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

export type LLMResult = { ok: true } | { ok: false; error: string }

export async function connectAnthropic(orgId: string, apiKey: string): Promise<LLMResult> {
  try {
    await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/llm/anthropic`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ api_key: apiKey }),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not validate the API key.') }
  }
}

// disconnectAnthropic removes the org's stored Anthropic key. Idempotent.
export async function disconnectAnthropic(orgId: string): Promise<LLMResult> {
  try {
    await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/llm/anthropic`, { method: 'DELETE' })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not remove the stored API key.') }
  }
}

// disconnectLLM is the "use the system Claude Code credentials" selection
// (local only): hold no org-level LLM credential of ANY provider, so the
// resolver falls back to the subscription/env path. That is two credentials and
// therefore two deletes — the server no longer has a single write that wipes
// both, because the one it had was an Anthropic route silently destroying AWS
// material. Both are idempotent, so this is safe on an org with nothing stored.
export async function disconnectLLM(orgId: string): Promise<LLMResult> {
  const anthropic = await disconnectAnthropic(orgId)
  if (!anthropic.ok) return anthropic
  return disconnectBedrock(orgId)
}
