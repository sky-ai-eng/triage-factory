import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import PromptsWorkspace from './PromptsWorkspace'
import { jsonBody } from '../test/apiResponse'

// What's under test is opening a linked trigger: the canvas and drawers are
// stand-ins, and the config panel shows which trigger it was opened on.
vi.mock('./BindingGraph', () => ({ default: () => null }))
vi.mock('./PromptDrawer', () => ({ default: () => null }))
vi.mock('./TaskRuleEditor', () => ({ default: () => null }))
vi.mock('./TriggerConfigPanel', () => ({
  default: (props: { open: boolean; trigger: { id: string } | null }) =>
    props.open ? <p>panel {props.trigger?.id}</p> : null,
}))
vi.mock('../hooks/useTeamRole', () => ({
  useTeamRole: () => ({ isViewer: () => false, loaded: true }),
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))

describe('PromptsWorkspace linked trigger', () => {
  beforeEach(() => {
    toastMock.error.mockReset()
  })

  it('opens the trigger the link names and tells the caller', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      ...jsonBody({ id: 'trig-1', kind: 'trigger', event_type: 'github:pr:ci_check_failed' }),
    })
    vi.stubGlobal('fetch', fetchMock)
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="trig-1" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(screen.getByText('panel trig-1')).toBeTruthy())
    expect(fetchMock).toHaveBeenCalledWith('/api/event-handlers/trig-1', expect.anything())
    expect(opened).toHaveBeenCalledTimes(1)
  })

  it('opens no panel for a rule, and still tells the caller', async () => {
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValue({ ok: true, status: 200, ...jsonBody({ id: 'rule-1', kind: 'rule' }) }),
    )
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="rule-1" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(opened).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^panel/)).toBeNull()
    expect(toastMock.error).toHaveBeenCalledTimes(1)
  })

  it('reports a trigger that is gone', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 404,
        ...jsonBody({ errors: [{ reason: 'NOT_FOUND', message: 'event handler not found' }] }),
      }),
    )
    const opened = vi.fn()
    render(
      <PromptsWorkspace teamId="team-a" ready openTriggerId="trig-gone" onTriggerOpened={opened} />,
    )

    await waitFor(() => expect(opened).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^panel/)).toBeNull()
    expect(toastMock.error).toHaveBeenCalledTimes(1)
  })
})
