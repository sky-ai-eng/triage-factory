import { describe, it, expect, vi, beforeEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { jsonBody } from '../test/apiResponse'
import ReviewOverlay from './ReviewOverlay'

// The pending review overlay offers its verbs (the verdict, edit, Submit) to a
// caller the deliverable's routes accept, and reads the review to one they
// refuse, the way it reads a resolved one.

function pendingReview(canWrite: boolean | undefined) {
  return {
    id: 'd1',
    kind: 'review',
    state: 'pending',
    url: '',
    ...(canWrite === undefined ? {} : { can_write: canWrite }),
    details: {
      owner: 'octo',
      repo: 'repo',
      pr_number: 7,
      review_body: 'Looks close.',
      review_event: 'COMMENT',
      commits_since_finalize: 2,
      comments: [],
    },
  }
}

// stubReads answers the overlay's two reads: the deliverable, and its diff,
// whose patch text arrives empty.
function stubReads(deliverable: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) =>
      url.endsWith('/diff')
        ? { ok: true, status: 200, text: async (): Promise<string> => '', headers: new Headers() }
        : { ok: true, status: 200, ...jsonBody(deliverable) },
    ),
  )
}

beforeEach(() => vi.restoreAllMocks())

async function open(canWrite: boolean | undefined) {
  stubReads(pendingReview(canWrite))
  render(<ReviewOverlay deliverableId="d1" open onClose={() => {}} />)
  expect(await screen.findByText('Looks close.')).toBeInTheDocument()
}

describe('ReviewOverlay', () => {
  it('reads a pending review to a caller who may not act on it', async () => {
    await open(false)
    expect(screen.getByRole('heading', { name: 'Pending Review' })).toBeInTheDocument()
    expect(screen.getByText(/view only/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /submit to github/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /request changes/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Refresh' })).not.toBeInTheDocument()
  })

  it('offers every verb to a writer, and to a response without the field', async () => {
    for (const canWrite of [true, undefined]) {
      cleanup()
      await open(canWrite)
      expect(screen.getByRole('button', { name: /submit to github/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /request changes/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Refresh' })).toBeInTheDocument()
      expect(screen.queryByText(/view only/i)).not.toBeInTheDocument()
    }
  })
})
