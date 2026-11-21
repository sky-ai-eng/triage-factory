import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, render, waitFor } from '@testing-library/react'
import type { Edge } from '@xyflow/react'
import BindingGraph from './BindingGraph'
import { jsonBody, listBody } from '../test/apiResponse'
import { reportFailedThroughError } from '../test/toastDouble'

// What's under test is the split and reconnect a sequence edge sends: each
// names the step index and the version the canvas read the steps at, and a
// refusal for a blueprint that changed since reloads the canvas and says so.
// The flow itself is a stand-in that hands the test the edges and the gesture
// callbacks the canvas gives it, since jsdom lays out no nodes to draw edges
// between.
const flow = vi.hoisted(() => ({
  props: null as null | {
    edges: Edge[]
    onReconnectStart: () => void
    onReconnect: (oldEdge: Edge, connection: Record<string, string | null>) => void
    onReconnectEnd: (event: MouseEvent, edge: Edge) => void
  },
}))
vi.mock('@xyflow/react', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@xyflow/react')>()),
  ReactFlow: (props: NonNullable<typeof flow.props>) => {
    flow.props = props
    return null
  },
  Background: () => null,
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn(), failed: vi.fn(), info: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock, backToOrigin: () => undefined }))
reportFailedThroughError(toastMock)

const fetchMock = vi.fn()

const prompt = (id: string) => ({
  id,
  name: id,
  body: '',
  source: 'user',
  usage_count: 0,
  created_at: '',
  updated_at: '',
})
const blueprint = (id: string) => ({ id, name: id, source: 'user', created_at: '', updated_at: '' })
const step = (bp: string, index: number, promptID: string, version: number) => ({
  blueprint_id: bp,
  step_index: index,
  step_prompt_id: promptID,
  brief: '',
  blueprint_version: version,
})

const changed = {
  ok: false,
  status: 409,
  ...jsonBody({
    errors: [
      {
        reason: 'VERSION_CONFLICT',
        message: 'this blueprint changed since you read its steps',
        field: 'version',
      },
    ],
  }),
}

/** Answers the canvas's reads with blueprint x, at version 4, stepping
 *  through a, b, and c, and blueprint t, at version 2, holding t0. Every
 *  other path answers from writes. */
function serve(writes: Record<string, () => unknown>) {
  const reads: Record<string, () => unknown> = {
    '/api/event-types': () => ({ ok: true, status: 200, ...jsonBody([]) }),
    '/api/blueprints/list': () => ({
      ok: true,
      status: 200,
      ...listBody([blueprint('x'), blueprint('t')]),
    }),
    '/api/prompts/list': () => ({
      ok: true,
      status: 200,
      ...listBody(['a', 'b', 'c', 't0'].map(prompt)),
    }),
    '/api/event-handlers/list': () => ({ ok: true, status: 200, ...listBody([]) }),
    '/api/blueprint-steps/list': () => ({
      ok: true,
      status: 200,
      ...listBody([
        step('x', 0, 'a', 4),
        step('x', 1, 'b', 4),
        step('x', 2, 'c', 4),
        step('t', 0, 't0', 2),
      ]),
    }),
  }
  fetchMock.mockImplementation(async (path: string) => {
    const answer = writes[path] ?? reads[path]
    if (answer) return answer()
    if (path.endsWith('/sources')) return { ok: true, status: 200, ...jsonBody({ sources: [] }) }
    throw new Error('unrouted ' + path)
  })
}

function sent(path: string): unknown[] {
  return fetchMock.mock.calls
    .filter(([p]) => p === path)
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)))
}

/** Renders the canvas and hands back the edge between a and b. */
async function edgeAB(): Promise<Edge> {
  render(<BindingGraph scope={{ teamId: '' }} scopeReady />)
  let edge: Edge | undefined
  await waitFor(() => {
    edge = flow.props?.edges.find((e) => e.source === 'p:a' && e.target === 'p:b')
    expect(edge).toBeDefined()
  })
  return edge!
}

beforeEach(() => {
  flow.props = null
  fetchMock.mockReset()
  toastMock.error.mockReset()
  toastMock.info.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})

describe('BindingGraph sequence edge cuts', () => {
  it('splits at the edge with the version the steps were read at', async () => {
    serve({ '/api/blueprints/x/split': () => ({ ok: true, status: 200, ...jsonBody({}) }) })
    const edge = await edgeAB()

    // Dropping the edge's head on empty space splits there.
    act(() => {
      flow.props!.onReconnectStart()
      flow.props!.onReconnectEnd(new MouseEvent('mouseup'), edge)
    })

    await waitFor(() =>
      expect(sent('/api/blueprints/x/split')).toEqual([{ at_step_index: 1, version: 4 }]),
    )
    await waitFor(() => expect(sent('/api/blueprint-steps/list')).toHaveLength(2))
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('reloads and says the blueprint changed when a split is refused for its version', async () => {
    serve({ '/api/blueprints/x/split': () => changed })
    const edge = await edgeAB()

    act(() => {
      flow.props!.onReconnectStart()
      flow.props!.onReconnectEnd(new MouseEvent('mouseup'), edge)
    })

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(toastMock.error.mock.calls[0][0]).toMatch(/changed since the canvas loaded it/)
    expect(toastMock.failed).not.toHaveBeenCalled()
    // The steps are read again, so the next gesture is decided against them.
    await waitFor(() => expect(sent('/api/blueprint-steps/list')).toHaveLength(2))
  })

  it('reconnects with the version and, refused for it, reloads and says the blueprint changed', async () => {
    serve({ '/api/blueprints/x/reconnect': () => changed })
    const edge = await edgeAB()

    // Dragging the edge's head onto t's entry reconnects x onto t.
    act(() => {
      flow.props!.onReconnectStart()
      flow.props!.onReconnect(edge, {
        source: 'p:a',
        target: 'p:t0',
        sourceHandle: null,
        targetHandle: null,
      })
    })

    await waitFor(() =>
      expect(sent('/api/blueprints/x/reconnect')).toEqual([
        { at_step_index: 1, target_blueprint_id: 't', version: 4 },
      ]),
    )
    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(toastMock.error.mock.calls[0][0]).toMatch(/changed since the canvas loaded it/)
    expect(toastMock.failed).not.toHaveBeenCalled()
    expect(toastMock.info).not.toHaveBeenCalled()
    await waitFor(() => expect(sent('/api/blueprint-steps/list')).toHaveLength(2))
  })
})
