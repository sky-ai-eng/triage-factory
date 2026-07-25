import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { initialWizardState, persistOrg, bedrockFormError } from './steps'
import type { WizardState } from './types'

// The org save carries CONFIG ONLY — credentials live on their own resources.
// That's what makes the App-XOR hazard structurally impossible: a wizard session
// that tried the GitHub PAT path first (typed a token, failed/abandoned the
// connect) then switched to the App path leaves the typed PAT lingering in the
// in-memory form, and the whole-form save behind the later poll-interval / model
// steps used to re-send it and 409 ("use the switch flow"), dead-ending the
// wizard. It can't now: there is no field to re-send it in. These tests assert
// the wire payload, since the payload is the whole property.

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

describe('persistOrg — the org save carries no credentials', () => {
  beforeEach(() => vi.restoreAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  // Parameterized over the App states that used to need a scrub, plus the
  // no-App case that used to be REQUIRED to send the token. All three now have
  // the same answer, which is the point: the payload no longer depends on
  // credential state at all.
  const cases: { name: string; over: Partial<WizardState> }[] = [
    { name: 'a live App is registered', over: { githubAppRegistered: true } },
    {
      name: 'a STAGED App is registered (the PAT is still live)',
      over: { githubAppRegistered: true, githubAppStaged: true, hasGitHubPat: true },
    },
    { name: 'no App is registered', over: { githubAppRegistered: false } },
  ]

  for (const { name, over } of cases) {
    it(`omits the typed org PAT when ${name}`, async () => {
      const bodies = captureSaveBodies()
      await persistOrg(loadedStateWithStalePat(over))
      const sent = bodies()
      expect(sent).toHaveLength(1)
      expect(sent[0]).not.toHaveProperty('github_pat')
      expect(sent[0]).not.toHaveProperty('jira_pat')
      // The rest of the form still round-trips, so an unrelated save isn't lossy.
      expect(sent[0]).toMatchObject({ github_base_url: 'https://github.com' })
    })
  }

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
