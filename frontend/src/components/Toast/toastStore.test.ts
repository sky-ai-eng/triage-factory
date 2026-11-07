import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiFetch } from '../../lib/apiClient'
import { rememberPageName } from '../../lib/pageNames'
import { toast, toastStore } from './toastStore'

// A failed request made on one page, with the viewer on `now` when it lands.
async function failedFrom(from: string, now: string): Promise<unknown> {
  window.history.pushState({}, '', from)
  let answer: (r: Response) => void = () => {}
  vi.stubGlobal(
    'fetch',
    vi.fn().mockReturnValue(new Promise<Response>((resolve) => (answer = resolve))),
  )
  const pending = apiFetch('/api/x', { method: 'POST' }).then(
    () => null,
    (e: unknown) => e,
  )
  window.history.pushState({}, '', now)
  answer({
    ok: false,
    status: 409,
    text: () =>
      Promise.resolve(JSON.stringify({ errors: [{ reason: 'CONFLICT', message: 'Taken.' }] })),
  } as unknown as Response)
  return pending
}

function shown(id: string) {
  return toastStore.getState().find((t) => t.id === id)
}

describe('toast.failed', () => {
  afterEach(() => {
    for (const t of toastStore.getState()) toastStore.dismiss(t.id)
    window.history.pushState({}, '', '/')
  })

  it("says what the server said, and links back by name once the viewer has left the request's page", async () => {
    rememberPageName('/orgs/o1/knowledge', 'Acme knowledge')
    const err = await failedFrom('/orgs/o1/knowledge?tab=shared', '/orgs/o1/board')
    const t = shown(toast.failed(err, 'The upload could not be saved.', 'Upload failed'))
    expect(t).toMatchObject({ level: 'error', body: 'Taken.', title: 'Upload failed' })
    expect(t?.action).toEqual({
      label: 'Back to Acme knowledge',
      href: '/orgs/o1/knowledge?tab=shared',
    })
  })

  it('links nowhere while the viewer is still on the page, whatever its query', async () => {
    const err = await failedFrom('/orgs/o1/board?filter=mine', '/orgs/o1/board')
    expect(shown(toast.failed(err, 'Could not claim the task.'))?.action).toBeUndefined()
  })

  it('links back without a name to a page that never named itself', async () => {
    const err = await failedFrom('/orgs/o1/unnamed', '/orgs/o1/board')
    expect(shown(toast.failed(err, 'Could not claim the task.'))?.action).toEqual({
      label: 'Back to the page this came from',
      href: '/orgs/o1/unnamed',
    })
  })

  it('takes the fallback, and links nowhere, for an error no request threw', () => {
    const t = shown(toast.failed(undefined, 'Could not read the file.'))
    expect(t).toMatchObject({ body: 'Could not read the file.' })
    expect(t?.action).toBeUndefined()
  })
})
