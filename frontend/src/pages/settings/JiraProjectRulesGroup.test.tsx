// The team's Jira surface, which is two gestures rather than one: WATCH a
// project from the org credential's live catalog, and — later, separately —
// ARM it by mapping its workflow's statuses. What is pinned here is that the
// first gesture costs one click and no mapping, that an unmapped project says
// so instead of hiding, and that arming edits the rules the save will send.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'

import JiraProjectRulesGroup from './JiraProjectRulesGroup'
import { emptyProject, projectIsArmed, type JiraProjectConfig } from './teamConfig'
import { jsonBody } from '../../test/apiResponse'

const CATALOG = [
  { key: 'SKY', name: 'Sky Platform' },
  { key: 'OPS', name: 'Operations' },
]

const STATUSES = [
  { id: '10000', name: 'To Do' },
  { id: '10001', name: 'In Progress' },
  { id: '10002', name: 'Done' },
]

// stubFetch answers the two reads this surface makes: the project catalog
// (a proxy list, so no total_count) and one project's statuses.
function stubFetch() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.startsWith('/api/jira/projects/list')) {
      return { ok: true, status: 200, ...jsonBody({ items: CATALOG, next_page_token: '' }) }
    }
    if (url.startsWith('/api/jira/statuses')) {
      return { ok: true, status: 200, ...jsonBody(STATUSES) }
    }
    // Nothing else on this surface reads; anything that appears is a mistake
    // in the test rather than a shape the component handles.
    throw new Error(`unexpected fetch: ${url || '(no url)'}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// Harness renders the controlled group with local state, the way its containers
// do, so a click that calls onChange is visible on the next render.
function Harness({ seed = [] as JiraProjectConfig[] }) {
  const [value, setValue] = useState<JiraProjectConfig[]>(seed)
  return (
    <>
      <JiraProjectRulesGroup value={value} onChange={setValue} connected bare />
      <output data-testid="armed">
        {value
          .filter(projectIsArmed)
          .map((p) => p.key)
          .join(',')}
      </output>
      <output data-testid="watched">{value.map((p) => p.key).join(',')}</output>
    </>
  )
}

// Braces, not a concise body: a function RETURNED from beforeEach is taken as
// the teardown hook and called with no arguments, which would run the fetch
// stub itself at cleanup.
beforeEach(() => {
  stubFetch()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('JiraProjectRulesGroup · watching', () => {
  it('lists the catalog as candidates', async () => {
    render(<Harness />)
    expect(await screen.findByText('Sky Platform')).toBeInTheDocument()
    expect(screen.getByText('Operations')).toBeInTheDocument()
  })

  it('watches a project in one click, with no mapping', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await screen.findByText('Sky Platform')

    const row = screen.getByText('Sky Platform').closest('div')!.parentElement!
    await user.click(within(row).getByRole('button', { name: 'Watch' }))

    expect(screen.getByTestId('watched')).toHaveTextContent('SKY')
    // Watched, and deliberately not armed — mapping is the next step, not a
    // precondition of this one.
    expect(screen.getByTestId('armed')).toHaveTextContent('')
  })

  it('says an unmapped project is unmapped, and prompts for the arming step', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[emptyProject('SKY')]} />)

    expect(screen.getByText('Statuses not mapped')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Expand project' }))
    expect(await screen.findByText(/Map its statuses below to arm it/)).toBeInTheDocument()
  })

  it('keeps a watched project the catalog no longer offers, so it can be dropped', async () => {
    render(<Harness seed={[emptyProject('GONE')]} />)
    await screen.findByText('Sky Platform')

    expect(screen.getByText('not visible to this credential')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Watching' })).toBeInTheDocument()
  })

  it('stops watching from the board', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[emptyProject('SKY')]} />)

    await user.click(screen.getByRole('button', { name: 'Stop watching SKY' }))
    expect(screen.getByTestId('watched')).toHaveTextContent('')
  })
})

// ruleBlock returns the container of one rule editor — the element holding its
// label, its write-target picker and its status chips. It is found by the
// rule's DESCRIPTION, which is unique on the page; the label and the status
// names are not ("Done" is a rule, a status chip in every rule, and a word in
// the copy).
const RULE_DESCRIPTIONS: Record<string, RegExp> = {
  Pickup: /Poll for unassigned tickets/,
  'In progress': /Count as actively being worked on/,
  Done: /Count as complete/,
}

function ruleBlock(label: string): HTMLElement {
  return screen.getByText(RULE_DESCRIPTIONS[label]).closest('div.space-y-2') as HTMLElement
}

describe('JiraProjectRulesGroup · arming', () => {
  it('fetches the project’s own statuses when its row is opened', async () => {
    const user = userEvent.setup()
    const fetchMock = stubFetch()
    render(<Harness seed={[emptyProject('SKY')]} />)

    await user.click(screen.getByRole('button', { name: 'Expand project' }))
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith('/api/jira/statuses?project=SKY', expect.anything()),
    )
    expect(await screen.findByText('3 statuses available')).toBeInTheDocument()
  })

  it('arms the project by mapping all three rules', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[emptyProject('SKY')]} />)
    await user.click(screen.getByRole('button', { name: 'Expand project' }))
    await screen.findByText('3 statuses available')

    // One status per rule. The first member of a write-target rule becomes its
    // canonical, so three clicks is a complete mapping. Each rule renders the
    // same three status chips, so the click has to be scoped to the rule block
    // its label heads rather than found by chip text alone.
    for (const [label, status] of [
      ['Pickup', 'To Do'],
      ['In progress', 'In Progress'],
      ['Done', 'Done'],
    ]) {
      const rule = ruleBlock(label)
      await user.click(within(rule).getByRole('button', { name: status }))
    }

    expect(screen.getByTestId('armed')).toHaveTextContent('SKY')
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })
})

describe('JiraProjectRulesGroup · statuses that vanished upstream', () => {
  // A project armed against a status that Jira has since deleted. Everything
  // downstream degrades quietly — polling skips the term, transitions to a
  // dead canonical fail — so this board is the only place a human is told.
  const withRetiredStatus = (): JiraProjectConfig => ({
    ...emptyProject('SKY'),
    pickup: {
      members: [
        { id: '10000', name: 'To Do' },
        { id: '99999', name: 'Retired' },
      ],
    },
    in_progress: {
      members: [{ id: '10001', name: 'In Progress' }],
      canonical: { id: '10001', name: 'In Progress' },
    },
    done: {
      members: [{ id: '10002', name: 'Done' }],
      canonical: { id: '10002', name: 'Done' },
    },
  })

  it('says nothing until the workflow has actually been read', async () => {
    render(<Harness seed={[withRetiredStatus()]} />)
    await screen.findByText('Sky Platform')
    // Collapsed, so no statuses fetched: an unread workflow is not evidence
    // that anything is missing, and claiming otherwise would cry wolf on
    // every board that has not been opened.
    expect(screen.queryByText(/status(es)? missing/i)).not.toBeInTheDocument()
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })

  it('flags the status once the workflow is read, over the armed state', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[withRetiredStatus()]} />)
    await user.click(screen.getByRole('button', { name: 'Expand project' }))

    expect(await screen.findByText('1 status missing')).toBeInTheDocument()
    expect(screen.getByText(/not in its Jira workflow any more/)).toBeInTheDocument()
    // "Ready" would be a lie: the rules name something Jira does not have.
    expect(screen.queryByText('Ready')).not.toBeInTheDocument()
  })

  it('removes the vanished status from the rules on click, and nothing else', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[withRetiredStatus()]} />)
    await user.click(screen.getByRole('button', { name: 'Expand project' }))
    await screen.findByText('1 status missing')

    await user.click(screen.getByRole('button', { name: 'Remove Retired from SKY' }))

    await waitFor(() => expect(screen.queryByText('1 status missing')).not.toBeInTheDocument())
    // The surviving statuses are untouched, so the project is armed again.
    expect(screen.getByTestId('armed')).toHaveTextContent('SKY')
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })

  it('does not flag a status that was merely renamed, since the id still resolves', async () => {
    const user = userEvent.setup()
    const renamed: JiraProjectConfig = {
      ...withRetiredStatus(),
      // Same id as the live "To Do"; only the stored spelling is stale.
      pickup: { members: [{ id: '10000', name: 'Backlog' }] },
    }
    render(<Harness seed={[renamed]} />)
    await user.click(screen.getByRole('button', { name: 'Expand project' }))

    expect(await screen.findByText('Ready')).toBeInTheDocument()
    expect(screen.queryByText(/status(es)? missing/i)).not.toBeInTheDocument()
  })
})
