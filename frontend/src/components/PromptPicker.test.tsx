import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
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

afterEach(() => {
  vi.unstubAllGlobals()
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

  it('keeps cached rows when a refetch fails, and still blames the filter', async () => {
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

    // With rows in hand, a filter that hides them all is a filter result — the
    // stale failure must not dress it up as a failed read.
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'zzz' } })
    expect(screen.getByText('No matches.')).toBeInTheDocument()
    expect(screen.getByText('No prompts match "zzz".')).toBeInTheDocument()
    expect(screen.queryByText("Couldn't load prompts.")).not.toBeInTheDocument()
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
