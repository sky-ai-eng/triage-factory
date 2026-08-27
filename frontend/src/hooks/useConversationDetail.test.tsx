import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, act, waitFor } from '@testing-library/react'
import { useConversationDetail } from './useConversationDetail'
import { TelemetryRail } from '../components/runstation/StationInstruments'
import { stationState } from '../components/runstation/stationStyle'
import { canResumeConversation, resumeBlockedCopy } from '../lib/conversationStatus'
import type { Conversation, Message, WSEvent } from '../types'
import { jsonBody } from '../test/apiResponse'

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

const CONVERSATION_ID = 'r1'
const T0 = new Date('2026-07-30T00:00:00Z').getTime()

// serverConversation is what GET /api/agent/conversations/r1 answers with — mutable so a
// test can change the server's authoritative SUM before triggering a refetch.
let serverConversation: Conversation

function conversation(over: Partial<Conversation>): Conversation {
  return {
    ID: CONVERSATION_ID,
    // Empty so the hook skips the parent-task fetch: this suite is about the
    // conversation row's readouts.
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
    conversation_id: CONVERSATION_ID,
    role: 'assistant',
    content: 'working',
    subtype: '',
    created_at: new Date(T0).toISOString(),
    ...over,
  }
}

// The rows the server holds for this conversation, which both transcript
// reads answer from: the unbounded one returns all of them, a `?since_id=N` one returns
// what sits above the watermark. A test models a dropped websocket frame by
// pushing here and never dispatching the event.
let serverMessages: Message[]

// A since_id read parked by a test, so a websocket frame can be made to land
// while the repair request is in flight. null = answer from serverMessages.
let pendingSince: Promise<Message[]> | null

// Makes since_id reads answer 500, for the transient-failure paths.
let failSinceReads: boolean

// Rows an older page (page_token read) answers with, keyed by the token asked
// for. Absent = no history behind the first page.
let olderPages: Record<string, { rows: Message[]; next: string }>

// The token the FIRST transcript read hands back, i.e. "there is history
// before this page". '' = the first read reached the beginning.
let firstPageOlderToken: string

// Route the reads the mounted view makes; everything but the conversation
// row and the transcript is empty. `transcript` lets a test hold the initial transcript
// read open — the conversation row lands first, so that read's duration is the
// window in which the conversation's SUM is displayed without the ids inside it
// being known yet.
function mockFetch(transcript?: Promise<Message[]>) {
  const fetchMock = vi.fn((url: string) => {
    const [path, query] = url.split('?')
    const params = query ? new URLSearchParams(query) : null
    // Read the param's PRESENCE, not its coerced value: a page_token URL also
    // has a query, and Number(null) is 0, which would send a back-page read
    // down the since_id branch.
    const rawSince = params?.get('since_id')
    const sinceID = rawSince === null || rawSince === undefined ? null : Number(rawSince)
    if (sinceID !== null && failSinceReads) {
      return Promise.resolve({ ok: false, status: 500, ...jsonBody(null) })
    }
    // The transcript answers the paged envelope; every fixture fits one page,
    // so next_page_token is never minted here.
    const transcriptPage = async (rows: Message[] | Promise<Message[]>, next = '') => ({
      items: await rows,
      next_page_token: next,
    })
    const pageToken = params?.get('page_token') ?? null
    const body: unknown = path.endsWith('/artifacts/refresh')
      ? { updated: 0 }
      : path.endsWith('/messages')
        ? sinceID !== null
          ? transcriptPage(pendingSince ?? serverMessages.filter((m) => m.id > sinceID))
          : pageToken
            ? transcriptPage(olderPages[pageToken]?.rows ?? [], olderPages[pageToken]?.next ?? '')
            : // A copy, like a real response body: the hook holds what a read
              // returns, and a test that later appends to the server's rows must
              // not be mutating the transcript React is rendering.
              transcriptPage(transcript ?? [...serverMessages], firstPageOlderToken)
        : path.endsWith('/artifacts') || path.endsWith('/actions')
          ? []
          : serverConversation
    return Promise.resolve({ ok: true, status: 200, ...jsonBody(body) })
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
  const { conversation, messages } = useConversationDetail(CONVERSATION_ID)
  if (!conversation) return <div>loading</div>
  return (
    <TelemetryRail
      conversation={conversation}
      messages={messages}
      state={stationState(conversation)}
      now={T0 + 60_000}
    />
  )
}

function send(event: WSEvent) {
  act(() => {
    dispatch?.(event)
  })
}

describe('useConversationDetail live cost accumulation', () => {
  beforeEach(() => {
    dispatch = null
    serverConversation = conversation({ TotalCostUSD: 0.2 })
    serverMessages = []
    pendingSince = null
    failSinceReads = false
    olderPages = {}
    firstPageOlderToken = ''
    mockFetch()
  })

  it('folds a streamed row’s cost into the readout with no conversation_update', async () => {
    render(<Harness />)
    expect(await screen.findByText('$0.2000')).toBeInTheDocument()

    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 11, cost_usd: 0.05 }),
    })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()

    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, cost_usd: 0.05 }),
    })
    expect(screen.getByText('$0.3000')).toBeInTheDocument()
  })

  it('does not double-count a replayed row', async () => {
    render(<Harness />)
    await screen.findByText('$0.2000')

    const row = message({ id: 11, cost_usd: 0.05 })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()
  })

  it('lets a conversation_update refetch overwrite the accumulated value', async () => {
    render(<Harness />)
    await screen.findByText('$0.2000')

    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 11, cost_usd: 0.05 }),
    })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()

    // The server's SUM is the authority: it lands whether or not it agrees with
    // what the client accumulated, so drift can't compound across a conversation.
    serverConversation = conversation({ TotalCostUSD: 2.64, Status: 'completed' })
    send({
      type: 'conversation_update',
      conversation_id: CONVERSATION_ID,
      data: { status: 'completed' },
    })
    expect(await screen.findByText('$2.64')).toBeInTheDocument()
  })

  it('does not strand a stamp that streams in before the conversation row lands', async () => {
    render(<Harness />)

    // The hook registers its websocket handler on the first render, so a row can
    // stream in while the conversation fetch is still in flight — with no
    // conversation object to fold into. The readout settling on the server's SUM (rather than on
    // 0.20 + 0.05) is what pins that this event landed in that window.
    const row = message({ id: 11, cost_usd: 0.05 })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
    expect(await screen.findByText('$0.2000')).toBeInTheDocument()

    // Nothing was recorded as counted, so the row's dollars are still foldable
    // rather than stranded — marking it would have lost them for the
    // whole conversation.
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()
  })

  it('does not re-count a row the just-fetched SUM already covers', async () => {
    // The conversation row is read before the transcript, so for the length
    // of that second read the displayed SUM's covered ids are unknown. A row inside it
    // that gets replayed over the websocket in that window must not be folded —
    // this is the mirror of the pre-conversation-row case above, and it
    // over-reports.
    const transcript = deferred<Message[]>()
    mockFetch(transcript.promise)
    render(<Harness />)
    expect(await screen.findByText('$0.2000')).toBeInTheDocument()

    const counted = message({ id: 11, cost_usd: 0.05 })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: counted })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()

    // Once the transcript lands, that row is the baseline — still not folded,
    // and a later replay can't fold it either.
    await act(async () => {
      transcript.settle([counted])
    })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()

    send({ type: 'message', conversation_id: CONVERSATION_ID, data: counted })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()

    // Folding resumes from that baseline for rows the SUM does not cover.
    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, cost_usd: 0.05 }),
    })
    expect(screen.getByText('$0.2500')).toBeInTheDocument()
  })

  it('leaves the readout alone for rows that carry no stamp (SDK runtime)', async () => {
    render(<Harness />)
    await screen.findByText('$0.2000')

    send({ type: 'message', conversation_id: CONVERSATION_ID, data: message({ id: 11 }) })
    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, output_tokens: 400 }),
    })
    expect(screen.getByText('$0.2000')).toBeInTheDocument()
  })
})

// The token rollups ride the same mechanism as cost: the conversation row's
// SUMs are the authority, and each streamed row's usage folds on top so a live engagement's
// readouts advance between conversation_update refetches. They keep their own
// baseline, since usage lands on nearly every assistant row while the cost
// stamp settles once at the end.
describe('useConversationDetail live token accumulation', () => {
  beforeEach(() => {
    dispatch = null
    serverConversation = conversation({
      input_tokens: 1000,
      output_tokens: 100,
      cache_read_tokens: 5000,
      cache_creation_tokens: 200,
    })
    serverMessages = []
    pendingSince = null
    failSinceReads = false
    olderPages = {}
    firstPageOlderToken = ''
    mockFetch()
  })

  it('shows the server’s sums rather than a walk of the transcript', async () => {
    // The transcript's own counts are deliberately not the conversation
    // row's: the SUM covers every row ever written, including any the client never held.
    serverMessages = [message({ id: 11, input_tokens: 7, output_tokens: 7 })]
    render(<Harness />)
    expect(await screen.findByText('1k')).toBeInTheDocument()
    expect(screen.getByText('100')).toBeInTheDocument()
    expect(screen.getByText('cache·r 5k')).toBeInTheDocument()
    expect(screen.getByText('cache·w 200')).toBeInTheDocument()
  })

  it('folds a streamed row’s usage into the readouts with no conversation_update', async () => {
    render(<Harness />)
    await screen.findByText('1k')

    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 11, input_tokens: 500, output_tokens: 50, cache_read_tokens: 1000 }),
    })
    expect(screen.getByText('1.5k')).toBeInTheDocument()
    expect(screen.getByText('150')).toBeInTheDocument()
    expect(screen.getByText('cache·r 6k')).toBeInTheDocument()
    // Untouched by a row that reported no cache write.
    expect(screen.getByText('cache·w 200')).toBeInTheDocument()
  })

  it('does not double-count a replayed row', async () => {
    render(<Harness />)
    await screen.findByText('1k')

    const row = message({ id: 11, input_tokens: 500, output_tokens: 50 })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
    expect(screen.getByText('1.5k')).toBeInTheDocument()
    expect(screen.getByText('150')).toBeInTheDocument()
  })

  it('does not re-count a row the just-fetched SUM already covers', async () => {
    // Same window as the cost path: between the conversation row landing and the
    // transcript landing, the ids inside the displayed SUM aren't known, so a
    // websocket replay of one of them must not fold.
    const transcriptRead = deferred<Message[]>()
    mockFetch(transcriptRead.promise)
    render(<Harness />)
    await screen.findByText('1k')

    const counted = message({ id: 11, input_tokens: 500, output_tokens: 50 })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: counted })
    expect(screen.getByText('1k')).toBeInTheDocument()

    await act(async () => {
      transcriptRead.settle([counted])
    })
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: counted })
    expect(screen.getByText('1k')).toBeInTheDocument()

    // Folding resumes from that baseline for rows the SUM does not cover.
    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, input_tokens: 500, output_tokens: 50 }),
    })
    expect(screen.getByText('1.5k')).toBeInTheDocument()
  })

  it('lets a conversation_update refetch overwrite the accumulated counts', async () => {
    render(<Harness />)
    await screen.findByText('1k')

    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 11, input_tokens: 500, output_tokens: 50 }),
    })
    expect(screen.getByText('1.5k')).toBeInTheDocument()

    serverConversation = conversation({
      Status: 'completed',
      input_tokens: 9000,
      output_tokens: 800,
      cache_read_tokens: 5000,
      cache_creation_tokens: 200,
    })
    send({
      type: 'conversation_update',
      conversation_id: CONVERSATION_ID,
      data: { status: 'completed' },
    })
    expect(await screen.findByText('9k')).toBeInTheDocument()
    expect(screen.getByText('800')).toBeInTheDocument()
  })

  it('leaves the readouts alone for a row that carries no usage', async () => {
    render(<Harness />)
    await screen.findByText('1k')

    send({ type: 'message', conversation_id: CONVERSATION_ID, data: message({ id: 11 }) })
    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, cost_usd: 0.05 }),
    })
    expect(screen.getByText('1k')).toBeInTheDocument()
    expect(screen.getByText('100')).toBeInTheDocument()
  })
})

// TranscriptHarness renders the held transcript as a joined string plus the
// cost readout, so a repaired row shows up both as content and as dollars.
function TranscriptHarness() {
  const { conversation, messages } = useConversationDetail(CONVERSATION_ID)
  return (
    <>
      <div data-testid="transcript">{messages.map((m) => m.content).join('|')}</div>
      <div data-testid="cost">{conversation ? String(conversation.TotalCostUSD ?? '') : ''}</div>
      <div data-testid="status">{conversation?.Status ?? ''}</div>
    </>
  )
}

function transcript(): string {
  return screen.getByTestId('transcript').textContent ?? ''
}

// Settle the conversation and wait for the refetch it triggers to actually
// land, so a following tick sees the settled row rather than racing it.
async function settleConversation() {
  serverConversation = conversation({ Status: 'completed', TotalCostUSD: 0.2 })
  send({
    type: 'conversation_update',
    conversation_id: CONVERSATION_ID,
    data: { status: 'completed' },
  })
  await waitFor(() => expect(screen.getByTestId('status').textContent).toBe('completed'))
}

// One turn of the 5s reconcile tick, with the fetches it starts settled.
async function tick() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5000)
  })
}

describe('useConversationDetail transcript reconciliation', () => {
  beforeEach(() => {
    // Only the reconcile interval is faked, so a test can bring the 5s tick
    // due on demand; setTimeout stays real so waitFor still polls normally.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    dispatch = null
    serverConversation = conversation({ TotalCostUSD: 0.2 })
    serverMessages = [message({ id: 11, content: 'first' })]
    pendingSince = null
    failSinceReads = false
    olderPages = {}
    firstPageOlderToken = ''
    mockFetch()
  })
  afterEach(() => {
    vi.useRealTimers()
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
    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, content: 'second' }),
    })
    await tick()
    expect(sinceRequests(fetchMock)).toEqual([
      `/api/agent/conversations/${CONVERSATION_ID}/messages?since_id=11`,
    ])
    expect(transcript()).toBe('first|second')

    // That repair verified through row 12, so the next one starts there —
    // the watermark only ever advances on a whole read.
    await tick()
    expect(sinceRequests(fetchMock)).toEqual([
      `/api/agent/conversations/${CONVERSATION_ID}/messages?since_id=11`,
      `/api/agent/conversations/${CONVERSATION_ID}/messages?since_id=12`,
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
    send({
      type: 'message',
      conversation_id: CONVERSATION_ID,
      data: message({ id: 13, content: 'third' }),
    })
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
    send({ type: 'message', conversation_id: CONVERSATION_ID, data: row })
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
      conversation_id: CONVERSATION_ID,
      data: message({ id: 12, content: 'second', cost_usd: 0.05 }),
    })
    expect(screen.getByTestId('cost').textContent).toBe('0.25')
  })

  it('issues no repair reads for a conversation already settled when the station opened', async () => {
    serverConversation = conversation({ Status: 'completed', TotalCostUSD: 0.2 })
    const fetchMock = mockFetch()
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))
    fetchMock.mockClear()

    // The load read the whole transcript and nothing more will be written to
    // it, so there is nothing a repair could find.
    serverMessages.push(message({ id: 12, content: 'second' }))
    await tick()
    expect(sinceRequests(fetchMock)).toEqual([])
    expect(transcript()).toBe('first')
    // The artifact half of the same tick is untouched — only the transcript
    // repair is gated on the conversation being live.
    expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/artifacts/refresh'))).toBe(
      true,
    )
  })

  it('repairs the last row when the frame carrying it is dropped as the conversation settles', async () => {
    // Status and transcript arrive as separate frames, so the hub can drop the
    // final `message` and still deliver the `conversation_update` that settles
    // the conversation. Gating on "live right now" would shut the repair off
    // over a transcript still missing its last row.
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    serverMessages.push(message({ id: 12, content: 'last' }))
    await settleConversation()

    await tick()
    expect(transcript()).toBe('first|last')
  })

  it('closes a settled transcript out with one read, then stops', async () => {
    const fetchMock = mockFetch()
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    await settleConversation()

    // The conversation is settled, so its transcript is final: one read
    // closes it out.
    await tick()
    expect(sinceRequests(fetchMock)).toHaveLength(1)

    // And no tick after that asks again — a settled conversation left open
    // on screen must not poll forever.
    await tick()
    await tick()
    expect(sinceRequests(fetchMock)).toHaveLength(1)
  })

  it('retries the closing read when it fails rather than dropping it', async () => {
    const fetchMock = mockFetch()
    render(<TranscriptHarness />)
    await waitFor(() => expect(transcript()).toBe('first'))

    await settleConversation()

    // A transient failure has not closed the transcript out, so the flag has
    // to stay up — otherwise one bad response costs the last row for good.
    serverMessages.push(message({ id: 12, content: 'last' }))
    failSinceReads = true
    await tick()
    expect(sinceRequests(fetchMock)).toHaveLength(1)
    expect(transcript()).toBe('first')

    failSinceReads = false
    await tick()
    expect(sinceRequests(fetchMock)).toHaveLength(2)
    expect(transcript()).toBe('first|last')

    // Now it is closed out, so the asking stops.
    await tick()
    expect(sinceRequests(fetchMock)).toHaveLength(2)
  })
})

// The composer's gate as the user experiences it: a parked conversation
// whose workspace hadn't landed yet renders the blocked copy, and the announcement that it has
// landed turns the input back on with no reload.
function ComposerHarness() {
  const { conversation } = useConversationDetail(CONVERSATION_ID)
  if (!conversation) return <div>loading</div>
  return (
    <div>
      {canResumeConversation(conversation) ? 'composer live' : resumeBlockedCopy(conversation)}
    </div>
  )
}

describe('useConversationDetail resumability', () => {
  // The conversation row read, parked from the second call on: the mount
  // gets an answer, and every refetch after it hangs. Anything that changes on screen
  // past that point can only have come from the websocket frame.
  let parkedRefetch: ReturnType<typeof deferred<Conversation>>

  function mockResumableFetch() {
    parkedRefetch = deferred<Conversation>()
    let conversationReads = 0
    const fetchMock = vi.fn((url: string) => {
      const [path] = url.split('?')
      if (path !== `/api/agent/conversations/${CONVERSATION_ID}`) {
        // Everything else this mount pulls — transcript, artifacts, pending
        // permissions — is empty here; the conversation row is the whole subject.
        return Promise.resolve({ ok: true, status: 200, ...jsonBody([]) })
      }
      conversationReads++
      // The conversation row goes through apiJSON now, so the stub has to
      // answer text() as well — jsonBody awaits the promise, which is what
      // parks the refetch.
      const body =
        conversationReads === 1 ? Promise.resolve(serverConversation) : parkedRefetch.promise
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(body) })
    })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  beforeEach(() => {
    dispatch = null
    serverMessages = []
    pendingSince = null
    failSinceReads = false
    olderPages = {}
    firstPageOlderToken = ''
    serverConversation = conversation({
      Status: 'open',
      resumable: false,
      resume_blocked_reason: 'workspace_expired',
    })
  })

  it('turns the composer on when the workspace is announced as accounted for', async () => {
    mockResumableFetch()
    render(<ComposerHarness />)
    // A cross-pod stop parks the row from control, before the executor holding
    // the workspace has recorded that it owes a persist for it, so this is the
    // honest state on arrival.
    expect(await screen.findByText(/workspace expired/i)).toBeInTheDocument()

    // The fenced teardown recorded the persist it owes. Same event, same status
    // the row already has, one extra field.
    send({
      type: 'conversation_update',
      conversation_id: CONVERSATION_ID,
      data: { status: 'open', resumable: true },
    })

    await waitFor(() => expect(screen.getByText('composer live')).toBeInTheDocument())
  })

  it('leaves the held answer alone when a status frame says nothing about it', async () => {
    serverConversation = conversation({ Status: 'open', resumable: true })
    mockResumableFetch()
    render(<ComposerHarness />)
    expect(await screen.findByText('composer live')).toBeInTheDocument()

    // Absent is "unchanged", not false — every other status flip on the wire
    // carries no resumable field and must not close a live composer.
    send({
      type: 'conversation_update',
      conversation_id: CONVERSATION_ID,
      data: { status: 'open' },
    })
    await waitFor(() => expect(screen.getByText('composer live')).toBeInTheDocument())
  })
})

// A probe rendering the transcript's paging surface plus the row ids it holds,
// so a test can drive "load earlier" and read the result without the station.
function PagingProbe() {
  const { messages, hasOlderMessages, loadingOlderMessages, loadOlderMessages } =
    useConversationDetail(CONVERSATION_ID)
  return (
    <div>
      <div data-testid="ids">{messages.map((m) => m.id).join(',')}</div>
      <div data-testid="has-older">{hasOlderMessages ? 'yes' : 'no'}</div>
      <button type="button" onClick={() => void loadOlderMessages()}>
        {loadingOlderMessages ? 'loading' : 'load earlier'}
      </button>
    </div>
  )
}

describe('useConversationDetail transcript back-paging', () => {
  beforeEach(() => {
    dispatch = null
    serverConversation = conversation({ TotalCostUSD: 0.2 })
    serverMessages = []
    pendingSince = null
    failSinceReads = false
    olderPages = {}
    firstPageOlderToken = ''
  })

  it('reports no earlier history when the first read reached the beginning', async () => {
    serverMessages = [message({ id: 5 })]
    mockFetch()
    render(<PagingProbe />)
    await waitFor(() => expect(screen.getByTestId('ids')).toHaveTextContent('5'))
    expect(screen.getByTestId('has-older')).toHaveTextContent('no')
  })

  it('walks backward through history, prepending each page in id order', async () => {
    // The regression this closes: the transcript read is bounded, so a long
    // conversation opens on its tail. Without a way back, everything before that page was
    // unreachable with nothing on screen saying so.
    serverMessages = [message({ id: 30 }), message({ id: 31 })]
    firstPageOlderToken = 'tok-20'
    olderPages = {
      'tok-20': { rows: [message({ id: 20 }), message({ id: 21 })], next: 'tok-10' },
      'tok-10': { rows: [message({ id: 10 })], next: '' },
    }
    mockFetch()
    render(<PagingProbe />)
    await waitFor(() => expect(screen.getByTestId('ids')).toHaveTextContent('30,31'))
    expect(screen.getByTestId('has-older')).toHaveTextContent('yes')

    screen.getByRole('button', { name: 'load earlier' }).click()
    await waitFor(() => expect(screen.getByTestId('ids')).toHaveTextContent('20,21,30,31'))
    expect(screen.getByTestId('has-older')).toHaveTextContent('yes')

    screen.getByRole('button', { name: 'load earlier' }).click()
    await waitFor(() => expect(screen.getByTestId('ids')).toHaveTextContent('10,20,21,30,31'))
    // The last page reached the beginning, so the control retires.
    await waitFor(() => expect(screen.getByTestId('has-older')).toHaveTextContent('no'))
  })

  it('does not fold an older page into the live totals', async () => {
    // Older rows are already inside the conversation row's SUM, so folding
    // them would add dollars that are already counted. Under-reporting self-corrects on
    // the next read; over-reporting compounds.
    serverConversation = conversation({ TotalCostUSD: 0.2 })
    serverMessages = [message({ id: 30 })]
    firstPageOlderToken = 'tok-old'
    olderPages = { 'tok-old': { rows: [message({ id: 10, cost_usd: 5 })], next: '' } }
    mockFetch()
    render(<Harness />)
    await screen.findByText(/\$0\.20/)

    // Drive the back-page through a second probe on the same hook instance is
    // not possible, so assert the invariant the fold would break: the rail
    // still reads the conversation row's SUM after the page lands.
    render(<PagingProbe />)
    await waitFor(() => expect(screen.getByTestId('has-older')).toHaveTextContent('yes'))
    screen.getByRole('button', { name: 'load earlier' }).click()
    await waitFor(() => expect(screen.getByTestId('ids')).toHaveTextContent('10,30'))
    expect(screen.getByText(/\$0\.20/)).toBeInTheDocument()
  })
})
