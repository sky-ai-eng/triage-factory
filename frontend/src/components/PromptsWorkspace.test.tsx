import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import PromptsWorkspace from './PromptsWorkspace'
import { jsonBody } from '../test/apiResponse'

// What's under test is opening a linked trigger: the canvas and drawers are
// stand-ins, and the config panel shows which trigger it was opened on and
// whether it is read-only.
vi.mock('./BindingGraph', () => ({ default: () => null }))
vi.mock('./PromptDrawer', () => ({ default: () => null }))
vi.mock('./TaskRuleEditor', () => ({ default: () => null }))
vi.mock('./TriggerConfigPanel', () => ({
  default: (props: { open: boolean; trigger: { id: string } | null; readOnly?: boolean }) =>
    props.open ? (
      <p>
        panel {props.trigger?.id} {props.readOnly ? 'read-only' : 'editable'}
      </p>
    ) : null,
}))
// The viewer edits team-a and is on no other team.
vi.mock('../hooks/useTeamRole', () => ({
  useTeamRole: () => ({
    isViewer: () => false,
    roleForTeam: (id: string | undefined) => (id === 'team-a' ? 'member' : null),
    loaded: true,
  }),
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))

function stubTrigger(body: unknown, ok = true, status = 200) {
  const fetchMock = vi.fn().mockResolvedValue({ ok, status, ...jsonBody(body) })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('PromptsWorkspace linked trigger', () => {
  beforeEach(() => {
    toastMock.error.mockReset()
  })

  it('opens the trigger the link names and reports its team', async () => {
    const fetchMock = stubTrigger({ id: 'trig-1', kind: 'trigger', team_id: 'team-a' })
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="trig-1" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(screen.getByText('panel trig-1 editable')).toBeTruthy())
    expect(fetchMock).toHaveBeenCalledWith('/api/event-handlers/trig-1', expect.anything())
    expect(opened).toHaveBeenCalledExactlyOnceWith('team-a')
  })

  it("opens a trigger of a team the viewer isn't on read-only", async () => {
    stubTrigger({ id: 'trig-x', kind: 'trigger', team_id: 'team-x' })
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="trig-x" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(screen.getByText('panel trig-x read-only')).toBeTruthy())
    expect(opened).toHaveBeenCalledExactlyOnceWith('team-x')
  })

  it('opens no panel for a rule, and still tells the caller', async () => {
    stubTrigger({ id: 'rule-1', kind: 'rule', team_id: 'team-a' })
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="rule-1" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(opened).toHaveBeenCalledExactlyOnceWith(null))
    expect(screen.queryByText(/^panel/)).toBeNull()
    expect(toastMock.error).toHaveBeenCalledTimes(1)
  })

  it('reports a trigger that is gone', async () => {
    stubTrigger(
      { errors: [{ reason: 'NOT_FOUND', message: 'event handler not found' }] },
      false,
      404,
    )
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="trig-gone" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(opened).toHaveBeenCalledExactlyOnceWith(null))
    expect(screen.queryByText(/^panel/)).toBeNull()
    expect(toastMock.error).toHaveBeenCalledTimes(1)
  })
})
