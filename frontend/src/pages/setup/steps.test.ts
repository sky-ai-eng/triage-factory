import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { initialWizardState, persistOrg, bedrockFormError } from './steps'
import type { WizardState } from './types'

// TFAC-410: a wizard session that tried the GitHub PAT path first (typed a
// token, failed/abandoned the connect) then switched to the App path leaves the
// typed PAT lingering in the in-memory org form. persistOrg (the whole-form save
// behind the poll-interval / model steps) must not re-send it once an App is
// registered, or the backend App-XOR guard 409s ("use the switch flow") and the
// wizard dead-ends. These tests assert the wire payload, since that's exactly
// what the guard inspects.

// A loaded wizard state carrying a stale typed org PAT in the form.
function loadedStateWithStalePat(over: Partial<WizardState> = {}): WizardState {
  const base = initialWizardState()
  return {
    ...base,
    orgLoaded: true,
    org: { ...base.org, github_url: 'https://github.com', github_pat: 'stale-pat-ghp_xxx' },
    ...over,
  }
}

// Stub fetch and capture each request's parsed JSON body.
function captureSaveBodies(): () => Record<string, unknown>[] {
  const bodies: Record<string, unknown>[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body ?? '{}')) as Record<string, unknown>)
      return new Response(JSON.stringify({}), { status: 200 })
    }),
  )
  return () => bodies
}

describe('persistOrg — App-XOR stale-PAT guard (TFAC-410)', () => {
  beforeEach(() => vi.restoreAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  it('omits a lingering github_pat from the save when a GitHub App is registered', async () => {
    const bodies = captureSaveBodies()
    await persistOrg(loadedStateWithStalePat({ githubAppRegistered: true }))
    const sent = bodies()
    expect(sent).toHaveLength(1)
    // saveOrgConfig sends `github_pat: form.github_pat || undefined`, so a scrubbed
    // '' drops out of the JSON entirely — the XOR guard never sees a PAT to reject.
    expect(sent[0]).not.toHaveProperty('github_pat')
    // The rest of the form still round-trips, so an unrelated save isn't lossy.
    expect(sent[0]).toMatchObject({ github_base_url: 'https://github.com' })
  })

  it('scrubs for a STAGED App too (PAT still live, but the switch flow owns it)', async () => {
    const bodies = captureSaveBodies()
    await persistOrg(
      loadedStateWithStalePat({
        githubAppRegistered: true,
        githubAppStaged: true,
        hasGitHubPat: true,
      }),
    )
    expect(bodies()[0]).not.toHaveProperty('github_pat')
  })

  it('sends a typed github_pat unchanged when NO App is registered', async () => {
    const bodies = captureSaveBodies()
    await persistOrg(loadedStateWithStalePat({ githubAppRegistered: false }))
    // No App ⇒ the PAT-path whole-form saves (e.g. the clone step) must still
    // carry a typed token; the scrub is scoped strictly to the App-registered case.
    expect(bodies()[0]).toHaveProperty('github_pat', 'stale-pat-ghp_xxx')
  })

  it('refuses to save (and makes no request) when the org load failed', async () => {
    const bodies = captureSaveBodies()
    await expect(
      persistOrg(loadedStateWithStalePat({ orgLoaded: false, githubAppRegistered: true })),
    ).rejects.toThrow(/reopen the GitHub step/)
    expect(bodies()).toHaveLength(0)
  })
})

// TFAC-68: the Bedrock credential form's input-layer validation, shared by
// the wizard key step's validate and the Settings section's Save gate. The
// rules mirror the backend's 422 shapes: region always required; the selected
// method's secrets required unless a credential for that SAME method is
// already stored (switching methods always needs the new secrets).
describe('bedrockFormError — Bedrock form validation (TFAC-68)', () => {
  function bedrockState(over: Partial<WizardState> = {}, org: Partial<WizardState['org']> = {}) {
    const base = initialWizardState()
    return {
      ...base,
      claudeProvider: 'bedrock' as const,
      org: { ...base.org, ...org },
      ...over,
    }
  }

  it('requires the region', () => {
    const s = bedrockState({}, { bedrock_region: ' ', bedrock_bearer_token: 'bdrk' })
    expect(bedrockFormError(s)).toMatch(/region/i)
  })

  it('requires a bearer token when nothing is stored', () => {
    const s = bedrockState({}, { bedrock_auth_method: 'bearer' })
    expect(bedrockFormError(s)).toMatch(/Bedrock API key/)
  })

  it('accepts a blank bearer token when a bearer credential is stored (keep current)', () => {
    const s = bedrockState(
      { bedrockConnected: true, bedrockStoredMethod: 'bearer' },
      { bedrock_auth_method: 'bearer' },
    )
    expect(bedrockFormError(s)).toBeNull()
  })

  it('requires the key pair when switching methods, even though a bearer is stored', () => {
    const s = bedrockState(
      { bedrockConnected: true, bedrockStoredMethod: 'bearer' },
      { bedrock_auth_method: 'access_keys' },
    )
    expect(bedrockFormError(s)).toMatch(/access key/i)
  })

  it('rejects a partial key pair', () => {
    const s = bedrockState({}, { bedrock_auth_method: 'access_keys', aws_access_key_id: 'AKIA' })
    expect(bedrockFormError(s)).toMatch(/both/i)
  })

  it('accepts a complete key pair', () => {
    const s = bedrockState(
      {},
      {
        bedrock_auth_method: 'access_keys',
        aws_access_key_id: 'AKIA',
        aws_secret_access_key: 'secret',
      },
    )
    expect(bedrockFormError(s)).toBeNull()
  })

  // TFAC-616: the IAM-role method carries no secret — the ARN is the only
  // method-specific field, always required (no "keep current"), and shape-gated
  // to look like an IAM role ARN before the server's real assume-role check.
  it('requires the role ARN in role mode', () => {
    const s = bedrockState({}, { bedrock_auth_method: 'role' })
    expect(bedrockFormError(s)).toMatch(/role ARN/i)
  })

  it('rejects a malformed role ARN', () => {
    const s = bedrockState({}, { bedrock_auth_method: 'role', bedrock_role_arn: 'not-an-arn' })
    expect(bedrockFormError(s)).toMatch(/valid IAM role ARN/i)
  })

  it('accepts a well-formed role ARN', () => {
    const s = bedrockState(
      {},
      {
        bedrock_auth_method: 'role',
        bedrock_role_arn: 'arn:aws:iam::123456789012:role/tf-bedrock',
      },
    )
    expect(bedrockFormError(s)).toBeNull()
  })

  it('still requires the region in role mode', () => {
    const s = bedrockState(
      {},
      {
        bedrock_auth_method: 'role',
        bedrock_role_arn: 'arn:aws:iam::123456789012:role/tf-bedrock',
        bedrock_region: '  ',
      },
    )
    expect(bedrockFormError(s)).toMatch(/region/i)
  })
})
