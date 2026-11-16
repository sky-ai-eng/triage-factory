import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, within, cleanup, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import RunStation, { type StationActions } from './RunStation'
import type { Conversation, Task } from '../../types'
import type { PendingPermission } from '../../lib/permissions'
import { jsonBody, listBody } from '../../test/apiResponse'
import { resetModelCatalogForTest } from '../../hooks/useModelCatalog'

// The dock is the surface this suite is about: whether the composer input is
// offered (and what stands in its place when it isn't), and how the approval
// affordance routes over the unresolved deliverable set.
function station(over: Partial<Conversation>, actions: Partial<StationActions> = {}) {
  const conversation = {
    id: 'r1',
    task_id: '',
    status: 'open',
    model: 'claude-opus-5',
    started_at: '2026-07-30T00:00:00Z',
    result_summary: '',
    ...over,
  } as Conversation
  return render(
    <MemoryRouter>
      <RunStation
        conversation={conversation}
        task={null}
        messages={[]}
        now={new Date('2026-07-30T00:01:00Z').getTime()}
        actions={{ onBack: () => {}, onMessage: async () => ({ sent: true }), ...actions }}
      />
    </MemoryRouter>,
  )
}

const composer = () => screen.queryByLabelText(/message to resume/i)

describe('RunStation composer gate', () => {
  beforeEach(() => {
    // The station's chrome reads deployment config; nothing here depends on
    // the answer, and the helper degrades to raw paths without it.
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })

  it('offers the input on a parked conversation the server says is resumable', () => {
    station({ status: 'open', resumable: true })
    expect(composer()).toBeInTheDocument()
  })

  it('offers it on a response that predates the field, falling back to status', () => {
    station({ status: 'open' })
    expect(composer()).toBeInTheDocument()
  })

  it('replaces it with the workspace copy when the workspace is gone', () => {
    // The bug the field exists for: from status alone this row is
    // indistinguishable from the one above, and every message it accepted
    // would come back 410.
    station({ status: 'open', resumable: false, resume_blocked_reason: 'workspace_expired' })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.getByText(/workspace expired/i)).toBeInTheDocument()
  })

  it('names the blueprint when that is what refused', () => {
    station({
      status: 'open',
      completed_at: '2026-07-16T11:00:00Z',
      outcome: 'finish',
      resumable: false,
      resume_blocked_reason: 'blueprint_concluded',
    })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.getByText(/blueprint/i)).toBeInTheDocument()
  })

  it('says nothing about resuming a failed conversation', () => {
    // Not a resumable status to begin with, so there is no offer to withdraw
    // and no explanation to give.
    station({ status: 'failed' })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.queryByText(/can’t be resumed|can’t take a follow-up/i)).not.toBeInTheDocument()
  })
})

describe('RunStation dock approval affordance', () => {
  // Radix FocusScope dispatches its unmount event from a zero-delay timer. On
  // a slow runner that timer can outlive the file's jsdom environment and
  // surface as an unhandled "dispatchEvent ... not of type 'Event'" error that
  // fails the whole run. Unmount first, then let the timer fire in-environment.
  afterEach(async () => {
    cleanup()
    await new Promise((resolve) => setTimeout(resolve, 0))
  })

  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })

  it('routes a lone unresolved item straight to its editor — no list detour', () => {
    const onOpenDeliverable = vi.fn()
    station(
      {
        status: 'running',
        has_unresolved_deliverables: true,
        unresolved_pr_count: 1,
        unresolved_review_count: 0,
        pending_deliverable_ids: ['pr1'],
      },
      { onOpenDeliverable },
    )
    fireEvent.click(screen.getByRole('button', { name: /open pr/i }))
    expect(onOpenDeliverable).toHaveBeenCalledWith('pr', 'pr1')
  })

  it('raises the deliverable-list popover for a mixed set and forwards a row pick', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        ...listBody([
          {
            id: 'pr1',
            kind: 'pull_request',
            provider: 'github',
            state: 'draft',
            target: 'org/repo#18',
            external_id: '18',
            url: 'https://gh/pr',
            details: null,
            created_at: '2026-07-30T00:00:00Z',
          },
          {
            id: 'rv1',
            kind: 'review',
            provider: 'github',
            state: 'pending',
            target: 'org/repo#19',
            external_id: '19',
            url: 'https://gh/rv',
            details: null,
            created_at: '2026-07-30T00:00:00Z',
          },
        ]),
      }),
    )
    const onOpenDeliverable = vi.fn()
    station(
      {
        status: 'running',
        has_unresolved_deliverables: true,
        unresolved_pr_count: 1,
        unresolved_review_count: 1,
        pending_deliverable_ids: ['pr1', 'rv1'],
      },
      { onOpenDeliverable },
    )

    fireEvent.click(screen.getByRole('button', { name: /review 2 items/i }))

    // The popover carries its own list instance (the telemetry rail renders
    // one too), so scope the row pick to the dialog.
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(await within(dialog).findByRole('button', { name: /open pull request/i }))
    expect(onOpenDeliverable).toHaveBeenCalledWith('pr', 'pr1')
  })
})

describe('RunStation retry readout', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })

  it('says when a queued run waiting out an unavailable provider tries again', () => {
    // `now` is 00:01; the hand-back set the next attempt four minutes later.
    station({ status: 'queued', next_attempt_at: '2026-07-30T00:05:00Z' })
    const at = new Date('2026-07-30T00:05:00Z').toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    })
    expect(screen.getByText(`Retrying at ${at}`)).toBeInTheDocument()
  })

  it('reads as an ordinary queued run once the time has passed', () => {
    station({ status: 'queued', next_attempt_at: '2026-07-30T00:00:30Z' })
    expect(screen.queryByText(/Retrying at/)).not.toBeInTheDocument()
  })

  it('ignores the stamp on a run that is no longer queued', () => {
    station({ status: 'running', next_attempt_at: '2026-07-30T00:05:00Z' })
    expect(screen.queryByText(/Retrying at/)).not.toBeInTheDocument()
  })
})

describe('RunStation for a reader who may not act', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })

  // A live conversation parked on a prompt, with every control's handler
  // wired, so the only thing deciding what renders is can_write.
  function liveStation(canWrite: boolean | undefined) {
    const conversation = {
      id: 'r1',
      task_id: '',
      status: 'running',
      model: 'claude-opus-5',
      started_at: '2026-07-30T00:00:00Z',
      result_summary: '',
      ...(canWrite === undefined ? {} : { can_write: canWrite }),
    } as Conversation
    return render(
      <MemoryRouter>
        <RunStation
          conversation={conversation}
          task={null}
          messages={[]}
          now={new Date('2026-07-30T00:01:00Z').getTime()}
          pendingPermissions={[
            { tool_call_id: 'toolu_1', tool_name: 'Bash', input: { command: 'ls' } },
          ]}
          actions={{
            onBack: () => {},
            onMessage: async () => ({ sent: true }),
            onCancel: () => {},
            onInterrupt: () => {},
            onResolvePermission: () => Promise.resolve(),
          }}
        />
      </MemoryRouter>,
    )
  }

  it('offers no composer, stop, or Allow/Deny when the server says the caller cannot write', () => {
    liveStation(false)
    expect(screen.queryByLabelText(/steer the agent/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /pause/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /cancel/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /allow/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /deny/i })).not.toBeInTheDocument()
    // The prompt itself still shows: the reader learns the agent is waiting.
    expect(screen.getByText('Allow Bash?')).toBeInTheDocument()
    expect(screen.getByText(/view only/i)).toBeInTheDocument()
  })

  it('offers every control to a writer, and to a response without the field', () => {
    for (const canWrite of [true, undefined]) {
      cleanup()
      liveStation(canWrite)
      expect(screen.getByLabelText(/steer the agent/i)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /pause/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /cancel/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /allow/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /deny/i })).toBeInTheDocument()
      expect(screen.queryByText(/view only/i)).not.toBeInTheDocument()
    }
  })
})

// The chat a person holds: the conversation their claim opened, its first
// message, and the model and effort every message names.
describe('RunStation chat', () => {
  const CATALOG = {
    items: [
      {
        key: 'opus',
        display_name: 'Opus 5',
        enabled: true,
        effort_levels: ['low', 'medium', 'high', 'xhigh', 'max'],
      },
      { key: 'sonnet', display_name: 'Sonnet 5', enabled: true, effort_levels: ['low', 'high'] },
      { key: 'haiku', display_name: 'Haiku 5', enabled: true, effort_levels: [] },
    ],
  }
  // The team enables two of the org's three.
  const TEAM_SETTINGS = {
    team_settings: { DefaultModel: 'sonnet', EnabledModels: ['sonnet', 'haiku'] },
  }

  beforeEach(() => {
    resetModelCatalogForTest()
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.endsWith('/models')) return Promise.resolve({ ok: true, ...jsonBody(CATALOG) })
        if (url.endsWith('/settings'))
          return Promise.resolve({ ok: true, ...jsonBody(TEAM_SETTINGS) })
        return Promise.reject(new Error('no network for ' + url))
      }),
    )
  })

  afterEach(async () => {
    cleanup()
    await new Promise((resolve) => setTimeout(resolve, 0))
  })

  function chat(
    over: Partial<Conversation>,
    {
      task = {},
      actions = {},
      pending = [],
    }: {
      task?: Partial<Task>
      actions?: Partial<StationActions>
      pending?: PendingPermission[]
    } = {},
  ) {
    const conversation = {
      id: 'c1',
      task_id: 't1',
      type: 'interactive',
      runtime: 'native',
      status: 'open',
      has_run: true,
      model: 'sonnet',
      next_message_model: 'sonnet',
      started_at: '2026-07-30T00:00:00Z',
      result_summary: '',
      ...over,
    } as Conversation
    const ui = (c: Conversation, prompts: PendingPermission[] = pending) => (
      <MemoryRouter>
        <RunStation
          conversation={c}
          task={
            {
              id: 't1',
              team_id: 'team1',
              conversation_id: 'c1',
              memory_pending: false,
              ...task,
            } as Task
          }
          messages={[]}
          now={new Date('2026-07-30T00:01:00Z').getTime()}
          actions={{ onBack: () => {}, onMessage: async () => ({ sent: true }), ...actions }}
          pendingPermissions={prompts}
        />
      </MemoryRouter>
    )
    const view = render(ui(conversation))
    return {
      ...view,
      rerenderWith: (next: Partial<Conversation>, prompts?: PendingPermission[]) =>
        view.rerender(ui({ ...conversation, ...next }, prompts)),
    }
  }

  const settingsButton = () => screen.getByRole('button', { name: /message settings/i })
  const send = () => screen.getByRole('button', { name: 'Send message' })
  const type = (label: RegExp, text: string) =>
    fireEvent.change(screen.getByLabelText(label), { target: { value: text } })

  it('shows a conversation that has not run as ready, and invites the first message', () => {
    chat({ has_run: false })
    expect(screen.getAllByText('READY').length).toBeGreaterThan(0)
    expect(screen.queryByText('IDLE')).not.toBeInTheDocument()
    expect(screen.getByLabelText('Start the chat')).toBeInTheDocument()
    expect(screen.getByText(/no messages yet/i)).toBeInTheDocument()
  })

  it('shows the memory wait, with the failed attempt when there was one, and keeps the composer', () => {
    chat({ has_run: false }, { task: { memory_pending: true } })
    expect(screen.getByRole('status')).toHaveTextContent(
      'Waiting for the previous conversation’s memory. Your message is queued and starts as soon as it lands.',
    )
    expect(screen.getByLabelText('Start the chat')).toBeInTheDocument()

    cleanup()
    chat(
      { has_run: false },
      {
        task: {
          memory_pending: true,
          memory_attempt: {
            outcome: 'failed',
            error_message: 'No background-jobs model is set.',
            started_at: '2026-07-30T00:00:30Z',
          },
        },
      },
    )
    expect(screen.getByRole('status')).toHaveTextContent(
      'Waiting for the previous conversation’s memory. The last attempt failed: No background-jobs model is set.',
    )
  })

  it('shows no memory wait on an earlier conversation of the task', () => {
    // The run a person took over: it owes the memory, and nothing waits on it.
    chat(
      {
        type: 'delegation',
        ended_at: '2026-07-30T00:00:40Z',
        ended_reason: 'taken_over',
        resumable: false,
        resume_blocked_reason: 'ended_taken_over',
      },
      { task: { memory_pending: true, conversation_id: 'c2' } },
    )
    expect(screen.queryByText(/Waiting for the previous conversation’s memory/)).toBeNull()
  })

  it('closes the settings while a permission prompt hides the composer', async () => {
    const view = chat({ status: 'running' })
    fireEvent.click(settingsButton())
    expect(await screen.findByRole('dialog')).toBeInTheDocument()

    view.rerenderWith({}, [
      { tool_call_id: 'toolu_1', tool_name: 'Bash', input: { command: 'ls' } },
    ])
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('holds the send while the starting model is one the team does not enable', async () => {
    chat({ next_message_model: 'opus', next_message_effort: 'high' })
    type(/message to resume/i, 'look at the flake')
    await waitFor(() => expect(settingsButton()).toHaveAccessibleName(/Opus 5 · .*unavailable/))
    expect(send()).toBeDisabled()

    fireEvent.click(settingsButton())
    fireEvent.click(await screen.findByRole('radio', { name: /Sonnet 5/ }))
    expect(send()).toBeEnabled()
  })

  it('shows a running blueprint step its own settings, read-only, until the blueprint is done', async () => {
    const view = chat({
      type: 'delegation',
      blueprint_run_id: 'br1',
      blueprint_run_status: 'running',
      next_message_model: 'opus',
    })
    expect(settingsButton()).toBeDisabled()
    await waitFor(() => expect(settingsButton()).toHaveAccessibleName(/Opus 5/))

    view.rerenderWith({ blueprint_run_status: 'completed' })
    expect(settingsButton()).toBeEnabled()
  })

  it('warns that a model switch re-reads the conversation while the cache is warm, and only then', async () => {
    const ahead = new Date(Date.now() + 4 * 60_000).toISOString()
    chat({ cache_warm_until: ahead })
    fireEvent.click(settingsButton())
    fireEvent.click(await screen.findByRole('radio', { name: /Haiku 5/ }))
    expect(
      screen.getByText(/re-reads the whole conversation at full input price/),
    ).toBeInTheDocument()
    // Back on the current model, nothing is lost.
    fireEvent.click(screen.getByRole('radio', { name: /Sonnet 5/ }))
    expect(screen.queryByText(/re-reads the whole conversation/)).not.toBeInTheDocument()

    cleanup()
    chat({ cache_warm_until: new Date(Date.now() - 60_000).toISOString() })
    fireEvent.click(settingsButton())
    fireEvent.click(await screen.findByRole('radio', { name: /Haiku 5/ }))
    expect(screen.queryByText(/re-reads the whole conversation/)).not.toBeInTheDocument()
  })

  it('keeps the draft through a refused send and shows the model fault in the settings', async () => {
    const onMessage = vi.fn(async () => ({
      sent: false as const,
      settingsErrors: [
        {
          reason: 'CONFLICT',
          message: 'sonnet is not among this team’s enabled models (haiku) — pick one of those',
          field: 'model',
        },
      ],
    }))
    chat({}, { actions: { onMessage } })
    type(/message to resume/i, 'look at the flake')
    fireEvent.click(send())

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('alert')).toHaveTextContent(
      /not among this team’s enabled models/,
    )
    expect(screen.getByLabelText(/message to resume/i)).toHaveValue('look at the flake')
    expect(onMessage).toHaveBeenCalledWith({
      text: 'look at the flake',
      model: 'sonnet',
      effort: '',
    })
  })
})
