import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import RepoPickerModal from './RepoPickerModal'
import * as repos from '../lib/githubRepos'
import type { GitHubRepo, RepoListPage } from '../lib/githubRepos'

// The picker fires several overlapping reads by design — a debounced search, a
// websocket-driven refetch, a poll while the org is still being
// discovered — so what these tests are about is what happens when those land out
// of order or outlive the component.

vi.mock('../lib/githubRepos', async () => {
  const actual = await vi.importActual<typeof import('../lib/githubRepos')>('../lib/githubRepos')
  return {
    ...actual,
    listReachableRepos: vi.fn(),
    refreshReachableRepos: vi.fn(),
  }
})

// The picker reads the org's App status on mount to pick its copy; neither test
// here is about that, and a real call would reach the stubbed-out fetch.
vi.mock('../lib/githubApp', () => ({
  LOCAL_DEFAULT_ORG_ID: '00000000-0000-0000-0000-000000000001',
  getGitHubAppStatus: vi.fn().mockResolvedValue({ app: null, installations: [] }),
  getGitHubAppInstallURL: vi.fn().mockResolvedValue(''),
}))

// The socket is a module-level singleton and a real connection attempt in jsdom
// is noise, but the picker's spinner ends on a ping — so the mock records the
// handler and a test fires the event itself.
let wsHandler: ((event: { type: string }) => void) | null = null
vi.mock('../hooks/useWebSocket', () => ({
  useWebSocket: (handler: (event: { type: string }) => void) => {
    wsHandler = handler
  },
}))

/** ping delivers the payload-free refresh announcement the server broadcasts
 *  when a reachable-set refresh lands. */
async function ping() {
  await act(async () => {
    wsHandler?.({ type: 'github_repos_updated' })
    await Promise.resolve()
  })
}

const listRepos = repos.listReachableRepos as unknown as ReturnType<typeof vi.fn>
const refresh = repos.refreshReachableRepos as unknown as ReturnType<typeof vi.fn>

function repo(fullName: string, githubId = `id-${fullName}`, repositoryId = ''): GitHubRepo {
  return {
    repository_id: repositoryId,
    github_id: githubId,
    full_name: fullName,
    html_url: '',
    description: '',
    language: '',
    pushed_at: '',
    private: false,
  }
}

function page(items: GitHubRepo[], overrides: Partial<RepoListPage> = {}): RepoListPage {
  return {
    items,
    next_page_token: '',
    total_count: items.length,
    status: 'ready',
    ...overrides,
  }
}

/** deferred hands back a promise plus the resolver, so a test can decide the
 *  ORDER two in-flight reads settle in — which is the whole subject here. */
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((r) => {
    resolve = r
  })
  return { promise, resolve }
}

function renderPicker() {
  return render(
    <RepoPickerModal inline hideFooter selected={[]} onSave={vi.fn()} onClose={vi.fn()} />,
  )
}

describe('RepoPickerModal', () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    listRepos.mockReset()
    refresh.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('keeps the newest search when an older one resolves after it', async () => {
    // Two reads in flight, settling in the wrong order: the newer "acme" answer
    // lands first, then the older unfiltered one. Without a sequence guard the
    // stale answer overwrites the fresh one and the user sees results for a
    // query they have already typed past.
    const first = deferred<RepoListPage>()
    const second = deferred<RepoListPage>()
    listRepos.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)

    renderPicker()
    // Let the mount fetch fire (debounced), then type — a second read.
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    fireEvent.change(screen.getByLabelText('Search repositories'), {
      target: { value: 'acme' },
    })
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    expect(listRepos).toHaveBeenCalledTimes(2)

    // Newer first, older second.
    await act(async () => {
      second.resolve(page([repo('acme/api')]))
      first.resolve(page([repo('other/tool'), repo('acme/api')]))
      await Promise.resolve()
    })

    expect(await screen.findByText('acme/api')).toBeInTheDocument()
    expect(screen.queryByText('other/tool')).not.toBeInTheDocument()
    expect(screen.getByText(/Showing 1 of 1/)).toBeInTheDocument()
  })

  it('keeps loading while the newest read is still in flight', async () => {
    // The other half of the same bug: a superseded read's `finally` must not
    // clear `loading`, or the skeleton disappears and an empty list shows while
    // the read that will fill it is still running.
    const first = deferred<RepoListPage>()
    const second = deferred<RepoListPage>()
    listRepos.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)

    renderPicker()
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    fireEvent.change(screen.getByLabelText('Search repositories'), {
      target: { value: 'acme' },
    })
    await act(async () => {
      vi.advanceTimersByTime(300)
    })

    // Only the superseded read settles.
    await act(async () => {
      first.resolve(page([repo('other/tool')]))
      await Promise.resolve()
    })
    // Nothing from the stale answer rendered, and the count line has not
    // committed to an answer yet.
    expect(screen.queryByText('other/tool')).not.toBeInTheDocument()

    await act(async () => {
      second.resolve(page([repo('acme/api')]))
      await Promise.resolve()
    })
    expect(await screen.findByText('acme/api')).toBeInTheDocument()
  })

  it('does not let a retired refresh deadline stop a later refresh spinner', async () => {
    // The control is disabled while refreshing, so two refreshes cannot overlap
    // by clicking twice — but a PING ends the spinner early and re-enables it,
    // which is how two deadlines come to exist at once. The first one, left
    // armed, would then report the second refresh — still running — as finished.
    listRepos.mockResolvedValue(page([repo('acme/api')]))
    refresh.mockResolvedValue(undefined)

    renderPicker()
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    await screen.findByText('acme/api')

    const click = async () => {
      fireEvent.click(screen.getByRole('button', { name: /^Refresh$/ }))
      await act(async () => {
        await Promise.resolve()
      })
    }

    await click()
    expect(screen.getByRole('button', { name: /Refreshing/ })).toBeInTheDocument()

    // A ping lands early — the refresh really did finish — so the spinner stops
    // and the control is live again well before its deadline would have fired.
    await act(async () => {
      vi.advanceTimersByTime(5_000)
    })
    await ping()
    expect(screen.getByRole('button', { name: /^Refresh$/ })).toBeInTheDocument()

    // Second refresh, its own deadline.
    await click()
    // Past the moment the FIRST deadline was set for. The spinner must still be
    // up: that deadline belongs to a refresh that has already been accounted for.
    await act(async () => {
      vi.advanceTimersByTime(27_000)
    })
    expect(screen.getByRole('button', { name: /Refreshing/ })).toBeInTheDocument()

    // The live deadline lands on its own schedule and does end it.
    await act(async () => {
      vi.advanceTimersByTime(5_000)
    })
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /^Refresh$/ })).toBeInTheDocument()
    })
  })

  it('leaves no refresh deadline pending after unmount', async () => {
    // A deadline that outlives the component holds its state setters alive for
    // the full 30s and then calls one. React 19 does not warn about that, so the
    // observable is the timer itself: after unmount nothing of this component's
    // may still be scheduled.
    listRepos.mockResolvedValue(page([repo('acme/api')]))
    refresh.mockResolvedValue(undefined)

    const { unmount } = renderPicker()
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    await screen.findByText('acme/api')
    fireEvent.click(screen.getByRole('button', { name: /^Refresh$/ }))
    await act(async () => {
      await Promise.resolve()
    })
    // The deadline is armed — otherwise this test would pass for the wrong
    // reason on a build where the refresh never sets one.
    expect(vi.getTimerCount()).toBeGreaterThan(0)

    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('re-asks the current search, not the last-loaded one, when a ping lands', async () => {
    // The ping means "the mirror moved, re-ask what you are showing". If it
    // re-asked the last SUCCESSFULLY LOADED query instead, a ping arriving while
    // a search is in flight would issue a newer request for the older question —
    // which then wins on arrival and leaves the list disagreeing with the box.
    listRepos.mockResolvedValue(page([repo('acme/api')]))

    renderPicker()
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    await screen.findByText('acme/api')
    expect(listRepos).toHaveBeenLastCalledWith('')

    // Type, but do not let the debounce fire — nothing has loaded "acme" yet, so
    // the last-loaded query is still the empty one.
    fireEvent.change(screen.getByLabelText('Search repositories'), {
      target: { value: 'acme' },
    })
    await ping()

    expect(listRepos).toHaveBeenLastCalledWith('acme')
  })

  it('renders the discovering state rather than an empty list', async () => {
    // "We have not looked yet" and "you have no repositories" are different
    // claims, and only the discriminator tells them apart.
    listRepos.mockResolvedValue(page([], { status: 'discovering', total_count: 0 }))

    renderPicker()
    await act(async () => {
      vi.advanceTimersByTime(300)
    })

    expect(await screen.findByText(/Discovering repositories/)).toBeInTheDocument()
    expect(screen.queryByText('No repositories found')).not.toBeInTheDocument()
  })

  it('matches the tracked set by GitHub id and reports new picks without a registry id', async () => {
    // GitHub renamed acme/api to acme/platform-api after the team tracked it;
    // the tracked entry still reads under its old name, and must still show
    // checked. acme/legacy has no GitHub id on file, so the server paired the
    // option with its registry row by name and the option carries that row.
    listRepos.mockResolvedValue(
      page([
        repo('acme/platform-api', '11'),
        repo('acme/legacy', '12', 'r-legacy'),
        repo('acme/web', '13'),
      ]),
    )
    const onSave = vi.fn()
    render(
      <RepoPickerModal
        inline
        selected={[
          { id: 'r-api', github_id: '11', owner: 'acme', repo: 'api' },
          { id: 'r-legacy', github_id: '', owner: 'acme', repo: 'legacy' },
        ]}
        onSave={onSave}
        onClose={vi.fn()}
      />,
    )
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    const box = async (name: string) =>
      (await screen.findByText(name)).closest('button') as HTMLButtonElement
    expect((await box('acme/platform-api')).getAttribute('aria-checked')).toBe('true')
    expect((await box('acme/legacy')).getAttribute('aria-checked')).toBe('true')
    expect((await box('acme/web')).getAttribute('aria-checked')).toBe('false')

    fireEvent.click(await box('acme/web'))
    fireEvent.click(await box('acme/legacy'))
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(onSave).toHaveBeenCalledWith([
      { id: 'r-api', github_id: '11', owner: 'acme', repo: 'api' },
      { id: '', github_id: '13', owner: 'acme', repo: 'web' },
    ])
  })

  it('shows a retired repository beside the one GitHub now serves under its name', async () => {
    // acme/api was deleted and recreated: the team still tracks the old row
    // (id 11, retired), and the reachable list offers the new repository
    // under the same name (id 21). They are two rows, and only the old one is
    // checked.
    listRepos.mockResolvedValue(page([repo('acme/api', '21'), repo('acme/web', '13')]))
    const onSave = vi.fn()
    const retired = {
      id: 'r-old',
      github_id: '11',
      owner: 'acme',
      repo: 'api',
      retired_at: '2026-10-20T12:00:00Z',
    }
    render(<RepoPickerModal inline selected={[retired]} onSave={onSave} onClose={vi.fn()} />)
    await act(async () => {
      vi.advanceTimersByTime(300)
    })

    const rows = (await screen.findAllByText('acme/api')).map(
      (el) => el.closest('button') as HTMLButtonElement,
    )
    expect(rows).toHaveLength(2)
    const [old, current] = rows
    expect(old.textContent).toContain('no longer on GitHub')
    expect(old.getAttribute('aria-checked')).toBe('true')
    expect(current.textContent).not.toContain('no longer on GitHub')
    expect(current.getAttribute('aria-checked')).toBe('false')

    // Swapping the old row for the new repository saves the new one alone.
    expect(old.textContent).toContain('Uncheck to stop tracking this one.')
    fireEvent.click(old)
    expect(old.textContent).not.toContain('Uncheck')
    fireEvent.click(current)
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(onSave).toHaveBeenCalledWith([{ id: '', github_id: '21', owner: 'acme', repo: 'api' }])
  })

  it('pairs an option with a tracked entry by id, never by a shared name alone', async () => {
    // The retired row once called acme/api has no GitHub id, and the option
    // under that name carries r-api, so the shared name pairs nothing.
    listRepos.mockResolvedValue(
      page([repo('acme/api', '11', 'r-api'), repo('acme/web', '13', 'r-web')]),
    )
    const onSave = vi.fn()
    const retired = {
      id: 'r-old',
      github_id: '',
      owner: 'acme',
      repo: 'api',
      retired_at: '2026-10-20T12:00:00Z',
    }
    render(
      <RepoPickerModal
        inline
        selected={[{ id: 'r-api', github_id: '11', owner: 'acme', repo: 'api' }, retired]}
        onSave={onSave}
        onClose={vi.fn()}
      />,
    )
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    const row = (el: HTMLElement) => el.closest('button')!
    const apis = (await screen.findAllByText('acme/api')).map(row)
    expect(apis).toHaveLength(2)
    const [old, current] = apis
    const web = row(await screen.findByText('acme/web'))
    const checked = () => [old, current, web].map((b) => b.getAttribute('aria-checked'))
    expect(checked()).toEqual(['true', 'true', 'false'])

    fireEvent.click(current)
    expect(checked()).toEqual(['true', 'false', 'false'])
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(onSave).toHaveBeenCalledWith([retired])
  })

  it('leaves a retired repository out of a search its name does not match', async () => {
    listRepos.mockResolvedValue(page([repo('acme/api', '21'), repo('acme/web', '13')]))
    const retired = {
      id: 'r-old',
      github_id: '11',
      owner: 'acme',
      repo: 'api',
      retired_at: '2026-10-20T12:00:00Z',
    }
    render(<RepoPickerModal inline selected={[retired]} onSave={vi.fn()} onClose={vi.fn()} />)
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    expect(await screen.findByText('no longer on GitHub')).toBeInTheDocument()

    listRepos.mockResolvedValue(page([repo('acme/web', '13')]))
    fireEvent.change(screen.getByLabelText('Search repositories'), { target: { value: 'web' } })
    await act(async () => {
      vi.advanceTimersByTime(300)
    })
    expect(await screen.findByText('acme/web')).toBeInTheDocument()
    expect(screen.queryByText('no longer on GitHub')).not.toBeInTheDocument()
  })
})
