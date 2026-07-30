import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import { useRunDetail } from './useRunDetail'
import { TelemetryRail } from '../components/runstation/StationInstruments'
import { stationState } from '../components/runstation/stationStyle'
import type { Conversation, Message, WSEvent } from '../types'

// The hook's live updates arrive through the singleton websocket. Mocking the
// module hands the test the handler the hook registered, so events can be
// dispatched synchronously with no socket in play.
let dispatch: ((event: WSEvent) => void) | null = null
vi.mock('./useWebSocket', () => ({
  useWebSocket: (handler: (event: WSEvent) => void) => {
    dispatch = handler
  },
  setPresenceView: vi.fn(),
}))

const RUN_ID = 'r1'
const T0 = new Date('2026-07-30T00:00:00Z').getTime()

// serverRun is what GET /api/agent/conversations/r1 answers with — mutable so a
// test can change the server's authoritative SUM before triggering a refetch.
let serverRun: Conversation

function conversation(over: Partial<Conversation>): Conversation {
  return {
    ID: RUN_ID,
    // Empty so the hook skips the parent-task fetch: this suite is about the
    // run row's readouts.
    TaskID: '',
    Status: 'running',
    Model: 'claude-opus-5',
    StartedAt: new Date(T0).toISOString(),
    ClaimedAt: new Date(T0).toISOString(),
    ResultSummary: '',
    artifact_count: 0,
    ...over,
  } as Conversation
}

function message(over: Partial<Message>): Message {
  return {
    id: 1,
    conversation_id: RUN_ID,
    role: 'assistant',
    content: 'working',
    subtype: 'text',
    created_at: new Date(T0).toISOString(),
    ...over,
  }
}

// Route the four reads the mounted view makes; everything but the run row is
// empty, so the only moving part is the cost readout. `transcript` lets a test
// hold the transcript read open — the run row lands first, so that read's
// duration is the window in which the run's SUM is displayed without the ids
// inside it being known yet.
function mockFetch(transcript?: Promise<Message[]>) {
  const fetchMock = vi.fn((url: string) => {
    const body: unknown = url.endsWith('/artifacts/refresh')
      ? { updated: 0 }
      : url.endsWith('/messages')
        ? (transcript ?? [])
        : url.endsWith('/artifacts')
          ? []
          : serverRun
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// A promise a test resolves by hand, to park one of the mocked reads.
function deferred<T>() {
  let settle!: (value: T) => void
  const promise = new Promise<T>((resolve) => {
    settle = resolve
  })
  return { promise, settle }
}

// Harness renders the station's instrument rail off the hook, so the assertions
// read the rendered cost the way the user sees it rather than hook internals.
function Harness() {
  const { run, messages } = useRunDetail(RUN_ID)
  if (!run) return <div>loading</div>
  return <TelemetryRail run={run} messages={messages} state={stationState(run)} now={T0 + 60_000} />
}

function send(event: WSEvent) {
  act(() => {
    dispatch?.(event)
  })
}

describe('useRunDetail live cost accumulation', () => {
  beforeEach(() => {
    dispatch = null
    serverRun = conversation({ TotalCostUSD: 0.2 })
    mockFetch()
  })
  afterEach(() => vi.unstubAllGlobals())

  it('folds a streamed row’s cost into the readout with no conversation_update', async () => {
    render(<Harness />)
    expect(await screen.findByText('$0.2000')).toBeInTheDocument()

    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 11, cost_usd: 0.05 }) })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()

    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 12, cost_usd: 0.05 }) })
    expect(screen.getByText('$0.3000')).toBeInTheDocument()
  })

  it('does not double-count a replayed row', async () => {
    render(<Harness />)
    await screen.findByText('$0.2000')

    const row = message({ id: 11, cost_usd: 0.05 })
    send({ type: 'message', conversation_id: RUN_ID, data: row })
    send({ type: 'message', conversation_id: RUN_ID, data: row })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()
  })

  it('lets a conversation_update refetch overwrite the accumulated value', async () => {
    render(<Harness />)
    await screen.findByText('$0.2000')

    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 11, cost_usd: 0.05 }) })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()

    // The server's SUM is the authority: it lands whether or not it agrees with
    // what the client accumulated, so drift can't compound across a run.
    serverRun = conversation({ TotalCostUSD: 2.64, Status: 'completed' })
    send({ type: 'conversation_update', conversation_id: RUN_ID, data: { status: 'completed' } })
    expect(await screen.findByText('$2.64')).toBeInTheDocument()
  })

  it('does not strand a stamp that streams in before the run row lands', async () => {
    render(<Harness />)

    // The hook registers its websocket handler on the first render, so a row can
    // stream in while the run fetch is still in flight — with no run object to
    // fold into. The readout settling on the server's SUM (rather than on
    // 0.20 + 0.05) is what pins that this event landed in that window.
    const row = message({ id: 11, cost_usd: 0.05 })
    send({ type: 'message', conversation_id: RUN_ID, data: row })
    expect(await screen.findByText('$0.2000')).toBeInTheDocument()

    // Nothing was recorded as counted, so the row's dollars are still foldable
    // rather than stranded — marking it would have lost them for the whole run.
    send({ type: 'message', conversation_id: RUN_ID, data: row })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()
  })

  it('does not re-count a row the just-fetched SUM already covers', async () => {
    // The run row is read before the transcript, so for the length of that
    // second read the displayed SUM's covered ids are unknown. A row inside it
    // that gets replayed over the websocket in that window must not be folded —
    // this is the mirror of the pre-run-row case above, and it over-reports.
    const transcript = deferred<Message[]>()
    mockFetch(transcript.promise)
    render(<Harness />)
    expect(await screen.findByText('$0.2000')).toBeInTheDocument()

    const counted = message({ id: 11, cost_usd: 0.05 })
    send({ type: 'message', conversation_id: RUN_ID, data: counted })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()

    // Once the transcript lands, that row is the baseline — still not folded,
    // and a later replay can't fold it either.
    await act(async () => {
      transcript.settle([counted])
    })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()

    send({ type: 'message', conversation_id: RUN_ID, data: counted })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()

    // Folding resumes from that baseline for rows the SUM does not cover.
    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 12, cost_usd: 0.05 }) })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()
  })

  it('leaves the readout alone for rows that carry no stamp (SDK runtime)', async () => {
    render(<Harness />)
    await screen.findByText('$0.2000')

    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 11 }) })
    send({
      type: 'message',
      conversation_id: RUN_ID,
      data: message({ id: 12, output_tokens: 400 }),
    })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()
  })
})
