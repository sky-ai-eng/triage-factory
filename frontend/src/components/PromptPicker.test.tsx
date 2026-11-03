import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import PromptPicker from './PromptPicker'
import type { Prompt } from '../types'
import { jsonBody, listBody } from '../test/apiResponse'

// What these tests are about: a picker showing no rows has two very different
// causes — the account has none, or the read failed — and they used to render
// the same pane. Someone who reads "No prompts yet" after a network blip
// concludes their prompts were deleted, when in fact they are untouched and a
// reopen would show them. So the assertions here are on which of the two the
// pane claims, not on the layout it claims it in.
//
// The global fetch is stubbed rather than apiListAll mocked, so the real
// apiClient path runs: an error envelope has to survive apiFetch → HttpError →
// httpErrorMessage to reach the pane, and that is the part worth pinning.

const fetchMock = vi.fn()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})

/** ok answers a `POST /…/list` read with one complete page. */
function ok(rows: Prompt[] = []) {
  return { ok: true, status: 200, ...listBody(rows) }
}

/** fails answers with the server's error envelope, which is where the pane's
 *  second line comes from. */
function fails(message: string) {
  return { ok: false, status: 500, ...jsonBody({ errors: [{ reason: 'INTERNAL', message }] }) }
}

function prompt(id: string, name: string): Prompt {
  return {
    id,
    name,
    body: 'do the thing',
    source: 'user',
    usage_count: 0,
    created_at: '',
    updated_at: '',
  }
}

/** deferred parks a read open so a test can assert on the in-flight render
 *  before letting it land. */
function deferred<T>() {
  let settle!: (v: T) => void
  const promise = new Promise<T>((res) => {
    settle = res
  })
  return { promise, settle }
}

function renderPicker() {
  return render(<PromptPicker open onSelect={() => {}} onClose={() => {}} />)
}

describe('PromptPicker empty vs failed', () => {
  it('reports an empty account when the read succeeds with no rows', async () => {
    fetchMock.mockResolvedValue(ok([]))
    renderPicker()

    expect(await screen.findByText('No prompts yet.')).toBeInTheDocument()
    // Nothing failed, so nothing to retry.
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument()
  })

  it('reports a failed read — with the server’s reason — instead of an empty account', async () => {
    fetchMock.mockResolvedValue(fails('the prompt store is unreachable'))
    renderPicker()

    expect(await screen.findByText("Couldn't load prompts.")).toBeInTheDocument()
    expect(screen.getByText('the prompt store is unreachable')).toBeInTheDocument()
    // The claim that would send the user off to re-create work they still have.
    expect(screen.queryByText('No prompts yet.')).not.toBeInTheDocument()
    // The rail echoes the failure rather than reading as an empty list.
    expect(screen.getByText('Failed to load.')).toBeInTheDocument()
  })

  it('falls back to a whole sentence when the failure carries no message', async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 502, ...jsonBody(undefined) })
    renderPicker()

    expect(await screen.findByText("Couldn't load prompts.")).toBeInTheDocument()
    expect(screen.getByText('The request failed.')).toBeInTheDocument()
  })

  it('keeps cached rows when a refetch fails, and a search that matches nothing is not a failure', async () => {
    fetchMock.mockResolvedValue(ok([prompt('p1', 'Fix the flake')]))
    const view = render(<PromptPicker open onSelect={() => {}} onClose={() => {}} />)
    expect(await screen.findByRole('option', { name: /Fix the flake/ })).toBeInTheDocument()

    // Reopen onto a server that has since gone down. Rows already in hand beat
    // an error message, so they stay on screen.
    fetchMock.mockResolvedValue(fails('the prompt store is unreachable'))
    view.rerender(<PromptPicker open={false} onSelect={() => {}} onClose={() => {}} />)
    view.rerender(<PromptPicker open onSelect={() => {}} onClose={() => {}} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    await act(async () => {})

    expect(screen.getByRole('option', { name: /Fix the flake/ })).toBeInTheDocument()
    expect(screen.queryByText("Couldn't load prompts.")).not.toBeInTheDocument()

    // The server back up, a search it answers with nothing is a search
    // result — the stale failure must not dress it up as a failed read.
    fetchMock.mockResolvedValue(ok([]))
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'zzz' } })
    expect(await screen.findByText('No matches.')).toBeInTheDocument()
    expect(screen.getByText('No prompts match "zzz".')).toBeInTheDocument()
    expect(screen.queryByText("Couldn't load prompts.")).not.toBeInTheDocument()
  })

  it('drops rows from another search when the read for this one fails', async () => {
    fetchMock.mockResolvedValue(ok([prompt('p1', 'Fix the flake')]))
    renderPicker()
    expect(await screen.findByRole('option', { name: /Fix the flake/ })).toBeInTheDocument()

    // Rows answering "" are not an answer to "deploy", so they do not stand
    // under it when its read fails.
    fetchMock.mockResolvedValue(fails('the prompt store is unreachable'))
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'deploy' } })
    expect(await screen.findByText("Couldn't load prompts.")).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /Fix the flake/ })).not.toBeInTheDocument()
  })

  it('Retry shows the skeleton, not a stale empty answer, while the read is in flight', async () => {
    // A fresh account, where "you have none" is the honest first answer.
    fetchMock.mockResolvedValueOnce(ok([]))
    const view = render(<PromptPicker open onSelect={() => {}} onClose={() => {}} />)
    expect(await screen.findByText('No prompts yet.')).toBeInTheDocument()

    // Reopen onto a server that has since gone down.
    fetchMock.mockResolvedValueOnce(fails('the prompt store is unreachable'))
    view.rerender(<PromptPicker open={false} onSelect={() => {}} onClose={() => {}} />)
    view.rerender(<PromptPicker open onSelect={() => {}} onClose={() => {}} />)
    const retry = await screen.findByRole('button', { name: 'Retry' })

    // Park the retry read. Until it answers, the picker knows nothing about
    // the account — so it must not go on claiming the empty answer it had
    // before the failure.
    const pending = deferred<ReturnType<typeof ok>>()
    fetchMock.mockReturnValueOnce(pending.promise)
    fireEvent.click(retry)

    expect(screen.getByRole('listbox')).toHaveAttribute('aria-busy', 'true')
    expect(screen.queryByText('No prompts yet.')).not.toBeInTheDocument()

    // And it lands back on a real answer.
    await act(async () => {
      pending.settle(ok([]))
    })
    expect(screen.getByText('No prompts yet.')).toBeInTheDocument()
  })

  it('Retry re-fires the read and recovers without a reopen', async () => {
    fetchMock.mockResolvedValueOnce(fails('the prompt store is unreachable'))
    renderPicker()

    const retry = await screen.findByRole('button', { name: 'Retry' })
    fetchMock.mockResolvedValueOnce(ok([prompt('p1', 'Fix the flake')]))
    fireEvent.click(retry)

    expect(await screen.findByRole('option', { name: /Fix the flake/ })).toBeInTheDocument()
    expect(screen.queryByText("Couldn't load prompts.")).not.toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})

// The same copy serves both sources through one noun, and the blueprint arm
// reaches it down a different road: three list reads in a Promise.all rather
// than one. So both the word and the path it arrives by are worth pinning —
// a picker that says "prompts" while listing blueprints names the wrong thing
// to go make.
describe('PromptPicker blueprints source', () => {
  function renderBlueprintPicker() {
    return render(<PromptPicker open source="blueprints" onSelect={() => {}} onClose={() => {}} />)
  }

  it('names blueprints when the read succeeds with no rows', async () => {
    fetchMock.mockResolvedValue(ok([]))
    renderBlueprintPicker()

    expect(await screen.findByText('No blueprints yet.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument()
  })

  it('names blueprints when the read fails, and offers the same Retry', async () => {
    fetchMock.mockResolvedValue(fails('the blueprint store is unreachable'))
    renderBlueprintPicker()

    expect(await screen.findByText("Couldn't load blueprints.")).toBeInTheDocument()
    expect(screen.getByText('the blueprint store is unreachable')).toBeInTheDocument()
    expect(screen.queryByText('No blueprints yet.')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })
})

// The search and the paging are the list routes': the picker sends what the
// reader typed and renders what the server answered, one page at a time. So
// these tests read the request bodies, and answer with rows a name match in
// the browser would have hidden.
describe('PromptPicker reads a page at a time, searched on the server', () => {
  /** Each call's path and parsed body, in order. */
  function calls(): Array<{ path: string; body: Record<string, unknown> | undefined }> {
    return fetchMock.mock.calls.map(([path, init]) => ({
      path: String(path),
      body: (init as RequestInit | undefined)?.body
        ? JSON.parse(String((init as RequestInit).body))
        : undefined,
    }))
  }

  /** route answers each path from a table, failing loudly on one it lacks. */
  function route(table: Record<string, (body: Record<string, unknown> | undefined) => unknown>) {
    fetchMock.mockImplementation(async (path: string, init?: RequestInit) => {
      const answer = table[path]
      if (!answer) throw new Error('unrouted ' + path)
      return answer(init?.body ? JSON.parse(String(init.body)) : undefined)
    })
  }

  it('sends the search and renders every row the server answers with', async () => {
    route({
      '/api/prompts/list': (body) =>
        body?.search
          ? ok([prompt('p1', 'Fix the flake'), prompt('p2', 'Rotate the keys')])
          : ok([prompt('p3', 'Something else')]),
    })
    renderPicker()
    expect(await screen.findByRole('option', { name: /Something else/ })).toBeInTheDocument()
    expect(calls()[0]).toEqual({ path: '/api/prompts/list', body: { page_size: 25 } })

    fireEvent.change(screen.getByRole('combobox'), { target: { value: '  flake ' } })
    // Rotate the keys does not contain "flake"; the server matched it anyway,
    // and what the server matched is what the picker shows.
    expect(await screen.findByRole('option', { name: /Rotate the keys/ })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: /Fix the flake/ })).toBeInTheDocument()
    expect(calls().at(-1)).toEqual({
      path: '/api/prompts/list',
      body: { search: 'flake', page_size: 25 },
    })
  })

  it('offers the next page, read with the same search and the token it was minted under', async () => {
    route({
      '/api/prompts/list': (body) =>
        body?.page_token === 'tok-1'
          ? ok([prompt('p2', 'Second flake')])
          : { ok: true, status: 200, ...listBody([prompt('p1', 'First flake')], 'tok-1', 2) },
    })
    renderPicker()
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'flake' } })
    await waitFor(() => expect(calls().at(-1)?.body).toMatchObject({ search: 'flake' }))

    fireEvent.click(await screen.findByRole('button', { name: 'Load more' }))
    expect(await screen.findByRole('option', { name: /Second flake/ })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: /First flake/ })).toBeInTheDocument()
    expect(calls().at(-1)?.body).toEqual({ search: 'flake', page_size: 25, page_token: 'tok-1' })
    // The last page minted no token, so there is nothing more to offer.
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument()
  })

  it('hides the row the caller is editing, and only that row', async () => {
    route({
      '/api/prompts/list': () =>
        ok([prompt('p1', 'Lint'), prompt('chain', 'The chain'), prompt('p3', 'Test')]),
    })
    render(
      <PromptPicker open onSelect={() => {}} onClose={() => {}} filter={(p) => p.id !== 'chain'} />,
    )
    expect(await screen.findByRole('option', { name: /Lint/ })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: /Test/ })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /The chain/ })).not.toBeInTheDocument()
  })

  it('reads a page of blueprints, the steps of that page alone, and the previewed prompts', async () => {
    const bp = (id: string, name: string) => ({
      id,
      name,
      source: 'user',
      usage_count: 0,
      created_at: '',
      updated_at: '',
    })
    const step = (blueprintID: string, index: number, promptID: string) => ({
      blueprint_id: blueprintID,
      step_index: index,
      step_prompt_id: promptID,
      brief: '',
      created_at: '',
    })
    route({
      '/api/blueprints/list': () => ok([bp('b1', 'Review'), bp('b2', 'Fix')] as never),
      '/api/blueprint-steps/list': () =>
        ok([step('b1', 0, 'sp1'), step('b1', 1, 'sp2'), step('b2', 0, 'sp3')] as never),
      '/api/prompts/sp1': () => ({
        ok: true,
        status: 200,
        ...jsonBody(prompt('sp1', 'Security pass')),
      }),
      '/api/prompts/sp2': () => ({
        ok: true,
        status: 200,
        ...jsonBody(prompt('sp2', 'Summarize')),
      }),
      '/api/prompts/sp3': () => ({
        ok: true,
        status: 200,
        ...jsonBody({ ...prompt('sp3', 'Fix it'), body: 'patch the failing test' }),
      }),
    })
    render(<PromptPicker open source="blueprints" onSelect={() => {}} onClose={() => {}} />)

    // The first row previews: its two steps, named by their prompts.
    expect(await screen.findByText('Security pass')).toBeInTheDocument()
    expect(screen.getByText('Summarize')).toBeInTheDocument()
    const seen = calls()
    expect(seen[0]).toEqual({ path: '/api/blueprints/list', body: { page_size: 25 } })
    expect(seen.find((c) => c.path === '/api/blueprint-steps/list')?.body).toMatchObject({
      blueprint_ids: ['b1', 'b2'],
    })
    // No prompt list is walked to name the steps: only the previewed ones are read.
    expect(seen.some((c) => c.path === '/api/prompts/list')).toBe(false)
    expect(seen.some((c) => c.path === '/api/prompts/sp3')).toBe(false)

    // A 1-step blueprint previews its sole step's body.
    fireEvent.click(screen.getByRole('option', { name: /Fix/ }))
    expect(await screen.findByText('patch the failing test')).toBeInTheDocument()
  })
})
