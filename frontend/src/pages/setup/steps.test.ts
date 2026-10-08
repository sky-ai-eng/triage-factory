import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  initialWizardState,
  persistOrgFields,
  bedrockFormError,
  linearActive,
  loadOrg,
  WIZARD_STEPS,
} from './steps'
import { isStepVisible, resumeIndex } from './resume'
import { jsonBody } from '../../test/apiResponse'
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

// The two model picks and the credential steps that must come first.
//
// The order is the requirement, not a preference: a picker asked before the
// credential can only offer models the org may turn out to have no way of
// running, and its availability badges have nothing to be about. And both picks
// are mandatory, because nothing falls back — a workspace through setup without
// them has background jobs that never run and a team whose unpinned steps refuse
// at dispatch.
describe('setup — credentials before the model picks, and both picks mandatory', () => {
  const indexOf = (id: string) => {
    const i = WIZARD_STEPS.findIndex((s) => s.id === id)
    if (i < 0) throw new Error(`${id} is missing from WIZARD_STEPS`)
    return i
  }
  const stepFor = (id: string) => WIZARD_STEPS[indexOf(id)]

  it('asks for the Claude credential before either model is chosen', () => {
    const credential = indexOf('org-claude-key')
    expect(credential).toBeLessThan(indexOf('org-background-jobs-model'))
    expect(credential).toBeLessThan(indexOf('team-model'))
    expect(indexOf('org-claude-source')).toBeLessThan(credential)
  })

  // Unset blocks in BOTH modes, and the mode difference is the seeded value
  // rather than a branch here: a local install's reads arrive pre-filled from
  // its own column defaults, so its steps are complete on arrival and it never
  // feels the gate.
  it('blocks on an unchosen background-jobs model', () => {
    const base = initialWizardState()
    expect(stepFor('org-background-jobs-model').isComplete(base)).toBe(false)
    expect(
      stepFor('org-background-jobs-model').isComplete({
        ...base,
        org: { ...base.org, background_jobs_model: 'claude-haiku-4-5-20251001' },
      }),
    ).toBe(true)
  })

  it('blocks on an unchosen team default model, and says what to do', () => {
    const base = initialWizardState()
    const step = stepFor('team-model')
    expect(step.isComplete(base)).toBe(false)
    expect(step.validate?.(base)).toMatch(/choose a default model/i)
    const chosen = { ...base, team: { ...base.team, default_model: 'claude-sonnet-5' } }
    expect(step.isComplete(chosen)).toBe(true)
    expect(step.validate?.(chosen)).toBeNull()
  })
})

// The Linear tracker's steps: which appear for which pick, how a returning
// org resumes, and what the access step's Continue sends.
describe('setup — the Linear tracker', () => {
  beforeEach(() => vi.restoreAllMocks())

  const stepFor = (id: string) => {
    const step = WIZARD_STEPS.find((s) => s.id === id)
    if (!step) throw new Error(`${id} is missing from WIZARD_STEPS`)
    return step
  }
  const visibleIds = (state: WizardState) =>
    WIZARD_STEPS.filter((s) => isStepVisible(s, state)).map((s) => s.id)
  const linearIds = ['org-linear-access', 'org-linear-poller', 'team-linear-teams']
  const jiraIds = ['org-jira-url', 'org-jira-mode', 'org-jira-access']

  it('shows no Linear step unless Linear is the pick', () => {
    for (const tracker of ['none', 'jira'] as const) {
      const ids = visibleIds({ ...initialWizardState(), tracker, linearConnected: true })
      for (const id of linearIds) expect(ids).not.toContain(id)
    }
  })

  it('shows the access step alone until the key is bound, then the cadence and team steps', () => {
    const picked = { ...initialWizardState(), tracker: 'linear' as const }
    let ids = visibleIds(picked)
    expect(ids).toContain('org-linear-access')
    expect(ids).not.toContain('org-linear-poller')
    expect(ids).not.toContain('team-linear-teams')
    for (const id of jiraIds) expect(ids).not.toContain(id)

    ids = visibleIds({ ...picked, linearConnected: true })
    expect(ids).toEqual(expect.arrayContaining(linearIds))
    expect(linearActive({ ...picked, linearConnected: true })).toBe(true)
  })

  it('resumes a fresh Linear pick on its access step, and blocks without a key', () => {
    const state = {
      ...initialWizardState(),
      tracker: 'linear' as const,
      githubReady: true,
      githubAccessTab: 'pat' as const,
    }
    expect(WIZARD_STEPS[resumeIndex(WIZARD_STEPS, state)].id).toBe('org-linear-access')
    const access = stepFor('org-linear-access')
    expect(access.validate?.(state)).toMatch(/Linear API key/)
    const typed = { ...state, org: { ...state.org, linear_api_key: 'lin_api_x' } }
    expect(access.validate?.(typed)).toBeNull()
    expect(access.isComplete({ ...state, linearConnected: true })).toBe(true)
  })

  it('blocks the team step on a half-mapped Linear team only', () => {
    const base = { ...initialWizardState(), tracker: 'linear' as const, linearConnected: true }
    const step = stepFor('team-linear-teams')
    expect(step.isComplete(base)).toBe(true)
    const half = {
      ...base,
      team: {
        ...base.team,
        linear_teams: [
          {
            id: 'lt-1',
            key: 'ENG',
            name: 'Engineering',
            pickup: { members: [{ id: 's1', name: 'Todo', type: 'unstarted' }] },
            in_progress: { members: [] },
            done: { members: [] },
          },
        ],
      },
    }
    expect(step.isComplete(half)).toBe(false)
    expect(step.validate?.(half)).toMatch(/half-mapped Linear team/)
  })

  it('names the workspace in the collapsed access bar', () => {
    const access = stepFor('org-linear-access')
    const s = initialWizardState()
    expect(access.collapsedSummary(s)).toBe('Not connected')
    expect(
      access.collapsedSummary({ ...s, linearConnected: true, linearWorkspaceUrlKey: 'acme' }),
    ).toBe('Connected · linear.app/acme')
    expect(stepFor('org-trackers').collapsedSummary({ ...s, tracker: 'linear' })).toBe('Linear')
  })

  // Continue performs the bind, then picks up the settings row's new token —
  // the bind wrote the workspace onto it, so the poll step's save right after
  // would otherwise conflict with this connect.
  it('binds the typed key, then folds in the workspace and the fresh version', async () => {
    const calls: { url: string; method: string; body: unknown }[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        const method = init?.method ?? 'GET'
        calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
        if (url.endsWith('/linear/access/credential')) {
          return {
            ok: true,
            status: 200,
            ...jsonBody({
              connected: true,
              auth_method: 'api_key',
              workspace_url_key: 'acme',
              bound_as: { name: 'ada', display_name: 'Ada' },
              connect_available: false,
              using_deployment_default: false,
            }),
          }
        }
        return { ok: true, status: 200, ...jsonBody({ version: 12 }) }
      }),
    )
    const base = initialWizardState()
    const state: WizardState = {
      ...base,
      orgLoaded: true,
      tracker: 'linear',
      org: { ...base.org, version: 11, linear_api_key: '  lin_api_x  ' },
    }
    const patched: Partial<WizardState>[] = []
    await stepFor('org-linear-access').persist({
      ...orgCtx(state, (p) => patched.push(p)),
      teamId: 'default',
      isLocal: true,
    })

    expect(calls[0]).toMatchObject({
      url: `/api/orgs/${ORG_ID}/linear/access/credential`,
      method: 'PUT',
      body: { api_key: 'lin_api_x' },
    })
    // The bind writes no column the settings page saves, so the version the
    // wizard holds stays current and nothing re-reads it.
    expect(calls).toHaveLength(1)
    expect(patched.at(-1)).toMatchObject({
      linearConnected: true,
      linearWorkspaceUrlKey: 'acme',
      linearBoundAs: 'Ada',
      org: { linear_api_key: '', version: 11 },
    })
  })

  it('saves only the Linear cadence from its poll step', async () => {
    const bodies = captureSaveBodies()
    await stepFor('org-linear-poller').persist({
      ...orgCtx(loadedStateWithStalePat()),
      teamId: 'default',
      isLocal: true,
    })
    expect(Object.keys(bodies()[0]).sort()).toEqual(['linear_poll_interval', 'version'])
  })

  // A returning org resumes on the tracker it connected; Linear wins when both
  // are, since the wizard sets up one and the other lives on in Settings.
  it('seeds the tracker pick from the connections on load', async () => {
    const answer = (linear: boolean, jira: boolean) =>
      vi.stubGlobal(
        'fetch',
        vi.fn(async (url: string) => {
          if (url.endsWith('/linear/access')) {
            return {
              ok: true,
              status: 200,
              ...jsonBody({ connected: linear, workspace_url_key: linear ? 'acme' : '' }),
            }
          }
          if (url === '/api/integrations/status') {
            return {
              ok: true,
              status: 200,
              ...jsonBody({ jira, jira_url: jira ? 'https://jira.example.com' : '' }),
            }
          }
          return { ok: true, status: 200, ...jsonBody({ version: 1, github_base_url: '' }) }
        }),
      )
    const ctx = { orgId: ORG_ID, teamId: 'default', isLocal: true }
    answer(true, true)
    expect(await loadOrg(ctx)).toMatchObject({ tracker: 'linear', linearConnected: true })
    answer(false, true)
    expect(await loadOrg(ctx)).toMatchObject({ tracker: 'jira', linearConnected: false })
    answer(false, false)
    expect((await loadOrg(ctx)).tracker).toBe('none')
  })

  // An unreadable Linear status is not "not connected": reading it that way
  // would hide a working connection and offer a form that binds over it.
  it('fails the load when the Linear status cannot be read', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url.endsWith('/linear/access')) {
          return { ok: false, status: 503, ...jsonBody({}) }
        }
        if (url === '/api/integrations/status') {
          return { ok: true, status: 200, ...jsonBody({ jira: false }) }
        }
        return { ok: true, status: 200, ...jsonBody({ version: 1, github_base_url: '' }) }
      }),
    )
    await expect(loadOrg({ orgId: ORG_ID, teamId: 'default', isLocal: true })).rejects.toThrow()
  })
})
