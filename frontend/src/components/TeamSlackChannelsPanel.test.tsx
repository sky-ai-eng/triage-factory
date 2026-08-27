import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import type { SlackChannelsResponse, SlackChannelView } from '../types'
import { toast } from './Toast/toastStore'

import TeamSlackChannelsPanel from './TeamSlackChannelsPanel'

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
// stateful-mock shape SSOSettings.test.tsx uses. The PUT handler reads
// `slackState` at call time, so a test can arm the post-save body by
// reassigning it before triggering the request.
let slackState: SlackChannelsResponse
let putRequests: string[][]
// When true, the NEXT channels GET fails (once) — used to exercise the
// post-primary-reassignment refresh failing while the reassignment itself
// still committed.
let failNextChannelsGet = false

function seed(resp: SlackChannelsResponse) {
  slackState = resp
  putRequests = []
  failNextChannelsGet = false
}

function errorResponse(): Response {
  return {
    ok: false,
    status: 500,
    json: async () => ({}),
    text: async () => '',
    clone() {
      return this as unknown as Response
    },
  } as unknown as Response
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

function installFetch() {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : String(input)
    const [path] = url.split('?')
    const method = init?.method ?? 'GET'

    if (path === `/api/slack/teams/${TEAM_ID}/channels` && method === 'GET') {
      if (failNextChannelsGet) {
        failNextChannelsGet = false
        return errorResponse()
      }
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

beforeEach(() => {
  seed({ role: 'admin', warnings: [], channels: [] })
  installFetch()
})

describe('TeamSlackChannelsPanel', () => {
  it('shows a quiet "connect a workspace" line when GET returns zero candidates', async () => {
    seed({ role: 'admin', warnings: [], channels: [] })
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin={false} />)

    expect(
      await screen.findByText(/connect a slack workspace in organization settings first/i),
    ).toBeInTheDocument()
  })

  it('sorts an unclaimed-with-activity row above a plain candidate and shows its chip + hint', async () => {
    const recentMention = new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString()
    seed({
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
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin={false} />)

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
    seed({ role: 'admin', warnings: [], channels: [channel({ channel_id: 'C0RAWID', name: '' })] })
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin={false} />)

    const row = await screen.findByRole('checkbox')
    expect(within(row).getByText('C0RAWID')).toBeInTheDocument()
    expect(within(row).queryByText('#C0RAWID')).not.toBeInTheDocument()
  })

  it('disables checkboxes and Save for a non-admin viewer, but keeps the list visible', async () => {
    seed({
      role: 'member',
      warnings: [],
      channels: [channel({ channel_id: 'C1', name: 'general' })],
    })
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin={false} />)

    const row = await screen.findByRole('checkbox')
    expect(row).toHaveAttribute('aria-disabled', 'true')
    expect(within(row).getByText('#general')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  })

  it("disables Make primary for an org admin who is not this team's admin", async () => {
    // orgIsAdmin gates whether the button renders at all; team `role` gates
    // whether it's safe to click right now — an org admin viewing as a
    // non-admin team member sees the button but can't fire it, mirroring the
    // row checkboxes (also disabled here) and the Save button.
    seed({
      role: 'member',
      warnings: [],
      channels: [
        channel({
          channel_id: 'C-shared',
          name: 'shared-channel',
          tracked: true,
          is_primary: false,
          tracked_by: [
            { team_id: 'other-team', team_name: 'Other Team', is_primary: true },
            { team_id: TEAM_ID, team_name: 'Team One', is_primary: false },
          ],
          source: 'tracked',
        }),
      ],
    })
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin />)

    expect(await screen.findByRole('button', { name: /make primary/i })).toBeDisabled()
  })

  it('claiming an unclaimed channel PUTs the union of the existing tracked set plus the new pick', async () => {
    seed({
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
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin={false} />)

    const newRow = await screen.findByRole('checkbox', { name: /newly-claimed/i })
    fireEvent.click(newRow)

    const saveBtn = screen.getByRole('button', { name: 'Save' })
    expect(saveBtn).not.toBeDisabled()
    fireEvent.click(saveBtn)

    await waitFor(() => expect(putRequests).toHaveLength(1))
    expect(new Set(putRequests[0])).toEqual(new Set(['C-tracked', 'C-new']))
  })

  it('renders the invite_required warning as a /invite instruction after save', async () => {
    seed({
      role: 'admin',
      warnings: [],
      channels: [
        channel({ channel_id: 'C-priv', name: 'secret-room', is_private: true, tracked: false }),
      ],
    })
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin={false} />)

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

  it('toasts an error when the post-reassignment refresh fails, instead of silently staying stale', async () => {
    seed({
      role: 'admin',
      warnings: [],
      channels: [
        channel({
          channel_id: 'C-shared',
          name: 'shared-channel',
          tracked: true,
          is_primary: false,
          tracked_by: [
            { team_id: 'other-team', team_name: 'Other Team', is_primary: true },
            { team_id: TEAM_ID, team_name: 'Team One', is_primary: false },
          ],
          source: 'tracked',
        }),
      ],
    })
    const errorSpy = vi.spyOn(toast, 'error')
    render(<TeamSlackChannelsPanel teamId={TEAM_ID} orgIsAdmin />)

    const makePrimaryBtn = await screen.findByRole('button', { name: /make primary/i })
    // The reassignment POST itself succeeds; only the follow-up GET (the
    // refresh) fails — the reassignment must not read as a no-op.
    failNextChannelsGet = true
    fireEvent.click(makePrimaryBtn)

    await waitFor(() =>
      expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('reload to see the change')),
    )
  })
})
