import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { initialWizardState, persistOrgFields, bedrockFormError, WIZARD_STEPS } from './steps'
import type { WizardState } from './types'

// The org save is FIELD-SCOPED: a step's persist names exactly the fields its
// card edits and nothing else rides the wire — not credentials (they live on
// their own resources; there is no field to send one in), and not the rest of
// the org form (so a step can't clobber a value written behind its back, like
// the base URL a credential bind just persisted). These tests assert the wire
// payload, since the payload is the whole property.

// A loaded wizard state carrying a stale typed org PAT in the form — the
// abandoned-PAT-attempt residue that the old whole-form save used to re-send.
function loadedStateWithStalePat(over: Partial<WizardState> = {}): WizardState {
  const base = initialWizardState()
  return {
    ...base,
    orgLoaded: true,
    org: { ...base.org, github_url: 'https://github.com', github_pat: 'stale-pat-ghp_xxx' },
    ...over,
  }
}

const ORG_ID = '00000000-0000-0000-0000-000000000001'

// Stub fetch and capture each request's parsed JSON body. The settings PATCH
// answers with the settings resource, so the stub returns a version — the
// persist folds it back into wizard state, and a stub that omitted it would
// let a version-dropping regression pass.
function captureSaveBodies(): () => Record<string, unknown>[] {
  const bodies: Record<string, unknown>[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body ?? '{}')) as Record<string, unknown>)
      return new Response(JSON.stringify({ version: 8 }), { status: 200 })
    }),
  )
  return () => bodies
}

// The persist's context: the state plus the patcher and the org the path names.
function orgCtx(state: WizardState, patch: (p: Partial<WizardState>) => void = () => {}) {
  return { state, patch, orgId: ORG_ID }
}

describe('persistOrgFields — each org step saves only what it owns', () => {
  beforeEach(() => vi.restoreAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  it('sends exactly the named fields plus the concurrency token', async () => {
    const bodies = captureSaveBodies()
    await persistOrgFields('jira_poll_interval')(orgCtx(loadedStateWithStalePat()))
    const sent = bodies()
    expect(sent).toHaveLength(1)
    expect(Object.keys(sent[0]).sort()).toEqual(['jira_poll_interval', 'version'])
  })

  // The old whole-form save had to scrub a lingering typed PAT out of the
  // payload per App state; field-scoping makes the hazard structural — there is
  // no field the token (or any neighbour value) could ride in.
  it('cannot carry a stale typed PAT or an unrelated field', async () => {
    const bodies = captureSaveBodies()
    await persistOrgFields('github_clone_protocol')(
      orgCtx(loadedStateWithStalePat({ githubAppRegistered: true })),
    )
    const sent = bodies()
    expect(sent[0]).not.toHaveProperty('github_pat')
    expect(sent[0]).not.toHaveProperty('github_base_url')
    expect(sent[0]).toMatchObject({ github_clone_protocol: 'https' })
  })

  it('refuses to save (and makes no request) when the org load failed', async () => {
    const bodies = captureSaveBodies()
    await expect(
      persistOrgFields('github_poll_interval')(
        orgCtx(loadedStateWithStalePat({ orgLoaded: false })),
      ),
    ).rejects.toThrow(/reopen the GitHub step/)
    expect(bodies()).toHaveLength(0)
  })

  // The settings row's concurrency token has to survive a save, or the wizard's
  // SECOND org step would assert the version it loaded with and 409 against its
  // own earlier write.
  it('folds the post-save version back into wizard state', async () => {
    captureSaveBodies()
    const patched: Partial<WizardState>[] = []
    await persistOrgFields('github_poll_interval')(
      orgCtx(loadedStateWithStalePat(), (p) => patched.push(p)),
    )
    expect(patched.at(-1)?.org?.version).toBe(8)
  })

  it('sends the loaded version so a concurrent admin save is a conflict, not a clobber', async () => {
    const bodies = captureSaveBodies()
    const state = loadedStateWithStalePat()
    state.org.version = 3
    await persistOrgFields('github_poll_interval')(orgCtx(state))
    expect(bodies()[0]).toMatchObject({ version: 3 })
  })

  // On a conflict the row is re-read and the fresh token folded into state
  // before the error surfaces, so the user's immediate retry can succeed —
  // "reload and re-apply" without a page reload.
  it('re-reads the fresh version on a 409 so a retry can succeed', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_url: string, init?: RequestInit) =>
        init?.method === 'PATCH'
          ? new Response(JSON.stringify({ errors: [] }), { status: 409 })
          : new Response(JSON.stringify({ version: 9 }), { status: 200 }),
      ),
    )
    const patched: Partial<WizardState>[] = []
    await expect(
      persistOrgFields('github_poll_interval')(
        orgCtx(loadedStateWithStalePat(), (p) => patched.push(p)),
      ),
    ).rejects.toThrow()
    expect(patched.at(-1)?.org?.version).toBe(9)
  })
})

// TFAC-68: the Bedrock credential form's input-layer validation, shared by
// the wizard key step's validate and the Settings section's Save gate. The
// rules mirror the backend's 400 shapes: the region and the selected shape's
// own credential are both always required — each shape's bind route REPLACES
// the credential, so there is no "leave blank to keep current" arm left on
// either side of the wire.
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

  it('still requires the bearer token when one is already stored (no keep-current)', () => {
    const s = bedrockState(
      { bedrockConnected: true, bedrockStoredMethod: 'bearer' },
      { bedrock_auth_method: 'bearer' },
    )
    expect(bedrockFormError(s)).toMatch(/Bedrock API key/)
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

// The team-default step's collapsed summary. A model is stored as an opaque
// catalog key, so the summary has to render it through the catalog — and the
// unset case is a real state (the form starts blank, and the wizard's own
// validate asks the user to choose), which must read as words rather than as
// "Default model: " with nothing after it.
describe('team-model step — collapsed summary', () => {
  const teamModelStep = () => {
    const step = WIZARD_STEPS.find((s) => s.id === 'team-model')
    if (!step) throw new Error('team-model step is missing from WIZARD_STEPS')
    return step
  }

  it('names the unmade choice rather than trailing off blank', () => {
    const state = initialWizardState()
    expect(state.team.default_model).toBe('')
    expect(teamModelStep().collapsedSummary(state)).toBe('Default model: Not chosen')
  })

  it('renders a chosen model through the catalog', () => {
    const base = initialWizardState()
    const state = { ...base, team: { ...base.team, default_model: 'claude-sonnet-5' } }
    // The catalog read has not landed in this unit, so the key itself is the
    // name — the point is that a chosen model is never rendered as blank.
    expect(teamModelStep().collapsedSummary(state)).toBe('Default model: claude-sonnet-5')
  })
})
