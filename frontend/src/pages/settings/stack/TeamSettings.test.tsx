import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import type { SlackChannelsResponse, SlackChannelView } from '../../../types'

// TeamSettings composes several self-fetching / heavy sibling sections
// unrelated to the Slack channels surface under test here — stub them out so
// a render only exercises team-settings load + the Slack section.
vi.mock('../../../hooks/useTeams', () => ({
  useTeams: () => ({ refresh: vi.fn() }),
}))
vi.mock('../../../components/RepoPickerModal', () => ({
  default: () => <div data-testid="repo-picker" />,
}))
vi.mock('../GitHubTeamGroup', () => ({
  default: () => <div data-testid="github-teams" />,
}))
vi.mock('../JiraProjectRulesGroup', () => ({
  default: () => <div data-testid="jira-projects" />,
}))
vi.mock('../../setup/ModelStep', () => ({
  TeamModelStep: () => <div data-testid="model-step" />,
}))

// useEntitlements gates the whole section — default on (loaded + licensed);
// individual tests flip `slack` off to exercise the "absent" gate.
const entMock = vi.hoisted(() => ({ slack: true, loaded: true }))
vi.mock('../../../hooks/useEntitlements', () => ({
  FeatureSlack: 'slack',
  useEntitlements: () => ({
    has: (f: string) => f === 'slack' && entMock.slack,
    loaded: entMock.loaded,
  }),
}))

import TeamSettings from './TeamSettings'

const TEAM_ID = 'team-1'

function channel(over: Partial<SlackChannelView> = {}): SlackChannelView {
  return {
    channel_id: 'C1',
    name: 'general',
    workspace_id: 'W1',
    is_private: false,
    bot_is_member: true,
    tracked: false,
    is_primary: false,
    tracked_by: [],
    source: 'sighting',
    ...over,
  }
}

// Mutable server state the fetch mock reads/writes, mirroring the
// stateful-mock shape SSOSettings.test.tsx uses. Reassigned per test via
// seedSlack(); the PUT/primary handlers mutate it in place so a re-GET (or a
// second render read) reflects the last write.
let slackState: SlackChannelsResponse
let putRequests: string[][]

function seedSlack(resp: SlackChannelsResponse) {
  slackState = resp
  putRequests = []
}

function jsonResponse(body: unknown): Response {
  return {
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
    clone() {
      return this as unknown as Response
    },
  } as unknown as Response
}

const TEAM_SETTINGS_BODY = {
  team_settings: {
    JiraProjects: [],
    AIReprioritizeThreshold: 0,
    AIPreferenceUpdateInterval: 0,
    DefaultModel: 'sonnet',
    AutoDelegateEnabled: true,
    BranchTemplate: 'tfac/<ticket-id>',
    PermissionAbsentGraceMS: 15000,
    PermissionAbsentAutodenyEnabled: true,
  },
  jira_projects: [],
  member_count: 1,
  role: 'admin',
}

function installFetch() {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : String(input)
    const [path] = url.split('?')
    const method = init?.method ?? 'GET'

    if (path === `/api/settings/team/${TEAM_ID}`) return jsonResponse(TEAM_SETTINGS_BODY)
    if (path === `/api/settings/team/${TEAM_ID}/repos`)
      return jsonResponse({ repos: [], role: 'admin' })
    if (path === `/api/settings/team/${TEAM_ID}/github-groups`) {
      return jsonResponse({ groups: [], candidates: [], role: 'admin' })
    }
    if (path === '/api/integrations/status') return jsonResponse({})

    if (path === `/api/slack/teams/${TEAM_ID}/channels` && method === 'GET') {
      return jsonResponse(slackState)
    }
    if (path === `/api/slack/teams/${TEAM_ID}/channels` && method === 'PUT') {
      const body = JSON.parse(init!.body as string) as { channel_ids: string[] }
      putRequests.push(body.channel_ids)
      return jsonResponse(slackState)
    }
    const primaryMatch = path.match(/^\/api\/slack\/channels\/(.+)\/primary$/)
    if (primaryMatch && method === 'POST') {
      return jsonResponse({ status: 'primary set' })
    }

    throw new Error(`unexpected fetch ${method} ${path}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function expandSlackSection() {
  fireEvent.click(await screen.findByRole('button', { name: /slack channels/i }))
}

beforeEach(() => {
  entMock.slack = true
  entMock.loaded = true
  seedSlack({ role: 'admin', warnings: [], channels: [] })
  installFetch()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('TeamSettings — Slack channels gating', () => {
  it('renders nothing when the org lacks the Slack entitlement', async () => {
    entMock.slack = false
    seedSlack({
      role: 'admin',
      warnings: [],
      channels: [channel({ channel_id: 'C1', name: 'general' })],
    })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)

    await screen.findByText('Repositories')
    expect(screen.queryByText('Slack channels')).not.toBeInTheDocument()
  })

  it('shows a quiet "connect a workspace" line when GET returns zero candidates', async () => {
    seedSlack({ role: 'admin', warnings: [], channels: [] })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)

    await expandSlackSection()
    expect(
      await screen.findByText(/connect a slack workspace in organization settings first/i),
    ).toBeInTheDocument()
  })
})

describe('TeamSettings — Slack channels list', () => {
  it('sorts an unclaimed-with-activity row above a plain candidate and shows its chip + hint', async () => {
    const recentMention = new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString()
    seedSlack({
      role: 'admin',
      warnings: [],
      channels: [
        channel({ channel_id: 'C-plain', name: 'random-plain' }),
        channel({
          channel_id: 'C-active',
          name: 'incidents-active',
          last_mention_at: recentMention,
        }),
      ],
    })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)
    await expandSlackSection()

    const rows = await screen.findAllByRole('checkbox')
    expect(rows).toHaveLength(2)
    // The unclaimed-with-activity row floats to the top of the unselected
    // portion, ahead of the plain (no-activity) candidate.
    expect(rows[0]).toHaveAccessibleName(/incidents-active/)
    expect(rows[1]).toHaveAccessibleName(/random-plain/)

    expect(within(rows[0]).getByText('unclaimed')).toBeInTheDocument()
    expect(within(rows[0]).getByText(/mentions seen/i)).toBeInTheDocument()
    expect(within(rows[1]).queryByText(/mentions seen/i)).not.toBeInTheDocument()
  })

  it('falls back to the raw channel_id (mono) when name is empty', async () => {
    seedSlack({
      role: 'admin',
      warnings: [],
      channels: [channel({ channel_id: 'C0RAWID', name: '' })],
    })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)
    await expandSlackSection()

    const row = await screen.findByRole('checkbox')
    expect(within(row).getByText('C0RAWID')).toBeInTheDocument()
    expect(within(row).queryByText('#C0RAWID')).not.toBeInTheDocument()
  })

  it('disables checkboxes and Save for a non-admin viewer, but keeps the list visible', async () => {
    seedSlack({
      role: 'member',
      warnings: [],
      channels: [channel({ channel_id: 'C1', name: 'general' })],
    })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)
    await expandSlackSection()

    const row = await screen.findByRole('checkbox')
    expect(row).toHaveAttribute('aria-disabled', 'true')
    expect(within(row).getByText('#general')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  })
})

describe('TeamSettings — Slack channels claim + save', () => {
  it('claiming an unclaimed channel PUTs the union of the existing tracked set plus the new pick', async () => {
    seedSlack({
      role: 'admin',
      warnings: [],
      channels: [
        channel({
          channel_id: 'C-tracked',
          name: 'already-tracked',
          tracked: true,
          is_primary: true,
          tracked_by: [{ team_id: TEAM_ID, team_name: 'Team One', is_primary: true }],
          source: 'tracked',
        }),
        channel({ channel_id: 'C-new', name: 'newly-claimed', tracked: false, tracked_by: [] }),
      ],
    })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)
    await expandSlackSection()

    const newRow = await screen.findByRole('checkbox', { name: /newly-claimed/i })
    fireEvent.click(newRow)

    const saveBtn = screen.getByRole('button', { name: 'Save' })
    expect(saveBtn).not.toBeDisabled()
    fireEvent.click(saveBtn)

    await waitFor(() => expect(putRequests).toHaveLength(1))
    expect(new Set(putRequests[0])).toEqual(new Set(['C-tracked', 'C-new']))
  })

  it('renders the invite_required warning as a /invite instruction after save', async () => {
    seedSlack({
      role: 'admin',
      warnings: [],
      channels: [
        channel({ channel_id: 'C-priv', name: 'secret-room', is_private: true, tracked: false }),
      ],
    })
    render(<TeamSettings isLocal={false} teamId={TEAM_ID} />)
    await expandSlackSection()

    fireEvent.click(await screen.findByRole('checkbox', { name: /secret-room/i }))

    // The mock's PUT handler reads `slackState` at call time, so the
    // post-save body (the channel now tracked, plus the private-channel
    // auto-join warning) must be armed before Save actually fires the
    // request — mirroring the real PUT response the backend returns
    // (post-change GET shape + warnings) that the component reads back.
    slackState = {
      role: 'admin',
      warnings: [{ channel_id: 'C-priv', reason: 'invite_required' }],
      channels: [
        channel({
          channel_id: 'C-priv',
          name: 'secret-room',
          is_private: true,
          tracked: true,
          is_primary: true,
          bot_is_member: false,
          tracked_by: [{ team_id: TEAM_ID, team_name: 'Team One', is_primary: true }],
          source: 'tracked',
        }),
      ],
    }
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(putRequests).toHaveLength(1))
    expect(
      await screen.findByText(/secret-room.*invite the bot manually.*\/invite @bot/i),
    ).toBeInTheDocument()
  })
})
