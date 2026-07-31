import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, act, waitFor } from '@testing-library/react'
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

// The rows the server holds for this run, which both transcript reads answer
// from: the unbounded one returns all of them, a `?since_id=N` one returns
// what sits above the watermark. A test models a dropped websocket frame by
// pushing here and never dispatching the event.
let serverMessages: Message[]

// A since_id read parked by a test, so a websocket frame can be made to land
// while the repair request is in flight. null = answer from serverMessages.
let pendingSince: Promise<Message[]> | null

// Route the reads the mounted view makes; everything but the run row and the
// transcript is empty. `transcript` lets a test hold the initial transcript
// read open — the run row lands first, so that read's duration is the window
// in which the run's SUM is displayed without the ids inside it being known yet.
function mockFetch(transcript?: Promise<Message[]>) {
  const fetchMock = vi.fn((url: string) => {
    const [path, query] = url.split('?')
    const sinceID = query ? Number(new URLSearchParams(query).get('since_id')) : null
    const body: unknown = path.endsWith('/artifacts/refresh')
      ? { updated: 0 }
      : path.endsWith('/messages')
        ? sinceID !== null
          ? (pendingSince ?? serverMessages.filter((m) => m.id > sinceID))
          : // A copy, like a real response body: the hook holds what a read
            // returns, and a test that later appends to the server's rows must
            // not be mutating the transcript React is rendering.
            (transcript ?? [...serverMessages])
        : path.endsWith('/artifacts')
          ? []
          : serverRun
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function sinceRequests(fetchMock: ReturnType<typeof mockFetch>): string[] {
  return fetchMock.mock.calls.map(([url]) => String(url)).filter((url) => url.includes('since_id'))
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
    serverMessages = []
    pendingSince = null
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

// TranscriptHarness renders the held transcript as a joined string plus the
// cost readout, so a repaired row shows up both as content and as dollars.
function TranscriptHarness() {
  const { run, messages } = useRunDetail(RUN_ID)
  return (
    <>
      <div data-testid="transcript">{messages.map((m) => m.content).join('|')}</div>
      <div data-testid="cost">{run ? String(run.TotalCostUSD ?? '') : ''}</div>
    </>
  )
}

function transcript(): string {
  return screen.getByTestId('transcript').textContent ?? ''
}

// One turn of the 5s reconcile tick, with the fetches it starts settled.
async function tick() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5000)
  })
}

describe('useRunDetail transcript reconciliation', () => {
  beforeEach(() => {
    // Only the reconcile interval is faked, so a test can bring the 5s tick
    // due on demand; setTimeout stays real so waitFor still polls normally.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    dispatch = null
    serverRun = conversation({ TotalCostUSD: 0.2 })
    serverMessages = [message({ id: 11, content: 'first' })]
    pendingSince = null
    mockFetch()
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('pulls in a row whose websocket frame never arrived', async () => {
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    // The socket is down: the server records two more rows and the client is
    // told about neither. Nothing on screen says a row is missing, which is
    // why this can't wait for a reload.
    serverMessages.push(
      message({ id: 12, content: 'second' }),
      message({ id: 13, content: 'third' }),
    )
    await tick()
    expect(transcript()).toBe('first|second|third')
  })

  it('asks from the last row it read, not the last one it was told about', async () => {
    const fetchMock = mockFetch()
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    // A frame arrives, so the client holds row 12 — but nothing has verified
    // the span between 11 and 12, so the watermark stays at what was read.
    serverMessages.push(message({ id: 12, content: 'second' }))
    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 12, content: 'second' }) })
    await tick()
    expect(sinceRequests(fetchMock)).toEqual([
      `/api/agent/conversations/${RUN_ID}/messages?since_id=11`,
    ])
    expect(transcript()).toBe('first|second')

    // That repair verified through row 12, so the next one starts there —
    // the watermark only ever advances on a whole read.
    await tick()
    expect(sinceRequests(fetchMock)).toEqual([
      `/api/agent/conversations/${RUN_ID}/messages?since_id=11`,
      `/api/agent/conversations/${RUN_ID}/messages?since_id=12`,
    ])
    expect(transcript()).toBe('first|second')
  })

  it('repairs a gap the socket then delivered rows above', async () => {
    // The failure mode a highest-id-held watermark cannot see: the hub drops
    // a frame for a slow client without disconnecting, so later frames keep
    // arriving and the client holds rows on both sides of the hole. Ids are
    // not contiguous, so nothing about what it holds says a row is missing.
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    serverMessages.push(
      message({ id: 12, content: 'second' }),
      message({ id: 13, content: 'third' }),
    )
    send({ type: 'message', conversation_id: RUN_ID, data: message({ id: 13, content: 'third' }) })
    expect(transcript()).toBe('first|third')

    await tick()
    expect(transcript()).toBe('first|second|third')
  })

  it('does not duplicate a row the socket delivers while the repair is in flight', async () => {
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    // Park the repair mid-flight, then let the frame it was sent to recover
    // arrive over the socket after all.
    const repair = deferred<Message[]>()
    pendingSince = repair.promise
    const row = message({ id: 12, content: 'second' })
    serverMessages.push(row)
    await tick()
    send({ type: 'message', conversation_id: RUN_ID, data: row })
    expect(transcript()).toBe('first|second')

    await act(async () => {
      repair.settle([row])
    })
    expect(transcript()).toBe('first|second')
  })

  it('folds a repaired row’s cost exactly like the frame that never arrived', async () => {
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))
    expect(screen.getByTestId('cost').textContent).toBe('0.2')

    serverMessages.push(message({ id: 12, content: 'second', cost_usd: 0.05 }))
    await tick()
    expect(screen.getByTestId('cost').textContent).toBe('0.25')

    // The row is held now, so a replay of it over the socket can't fold twice.
    send({
      type: 'message',
      conversation_id: RUN_ID,
      data: message({ id: 12, content: 'second', cost_usd: 0.05 }),
    })
    expect(screen.getByTestId('cost').textContent).toBe('0.25')
  })

  it('issues no repair reads once the run has settled', async () => {
    serverRun = conversation({ Status: 'completed', TotalCostUSD: 0.2 })
    const fetchMock = mockFetch()
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))
    fetchMock.mockClear()

    // A settled run streams nothing, so there is nothing to repair.
    serverMessages.push(message({ id: 12, content: 'second' }))
    await tick()
    expect(sinceRequests(fetchMock)).toEqual([])
    expect(transcript()).toBe('first')
    // The artifact half of the same tick is untouched — only the transcript
    // repair is gated on the run being live.
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/artifacts/refresh'))).toBe(
      true,
    )
  })
})
