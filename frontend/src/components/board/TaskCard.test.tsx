import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { TaskCard } from './TaskCard'

// One card for every lane state. These pin what each state spends a line on,
// and what it does not: the summary shows in every state, a permission takes
// the activity row's slot, an approval spends no line and brightens the
// frame, and a settled run folds its row into the footer.

function renderCard(props: Partial<React.ComponentProps<typeof TaskCard>> = {}) {
  return render(
    <MemoryRouter>
      <TaskCard title="Serialize the cgroup read" lifecycle="queued" {...props} />
    </MemoryRouter>,
  )
}

/** The warm share the corner brackets are drawn at. */
function bracketWarm(): string {
  const arm = document.querySelector('.tc > span[aria-hidden]') as HTMLElement
  return arm.style.borderLeft
}

describe('TaskCard, queued', () => {
  it('shows the age, the summary and the queued mark, and no run', () => {
    renderCard({
      age: '4h ago',
      summary: 'Two readers hit the cgroup file at once.',
      entity: 'acme/api#761',
      entityHref: 'https://github.com/acme/api/issues/761',
    })
    expect(screen.getByText('4h ago')).toBeInTheDocument()
    expect(screen.getByText('Two readers hit the cgroup file at once.')).toBeInTheDocument()
    expect(document.querySelector('[data-mark="queued"]')).not.toBeNull()
    expect(screen.queryByText('View run')).not.toBeInTheDocument()
    // The title is the link to the thing it names, with the out-arrow beside it.
    expect(screen.getByRole('link', { name: 'Serialize the cgroup read' })).toHaveAttribute(
      'href',
      'https://github.com/acme/api/issues/761',
    )
    expect(document.querySelector('.tc-out')).not.toBeNull()
  })

  it('is plain text without somewhere to go', () => {
    renderCard({ age: '4h ago' })
    expect(screen.queryByRole('link', { name: 'Serialize the cgroup read' })).toBeNull()
    expect(document.querySelector('.tc-out')).toBeNull()
  })

  it('carries the artifacts a requeue handed back, and warms the frame when one waits', () => {
    renderCard({
      age: '4h ago',
      href: '/runs/c1',
      artifacts: { branch: 1, pulls: 1 },
      pending: { pulls: 1 },
    })
    expect(screen.getByTitle("Open the run's artifacts")).toHaveAttribute('href', '/runs/c1')
    expect(bracketWarm()).toContain('90%')
  })

  it('draws the summary in hairlines while it is still being written', () => {
    renderCard({ age: '4h ago', summaryPending: true })
    expect(
      document.querySelectorAll('.tc span[aria-hidden] > span[style*="tf-draw"]'),
    ).toHaveLength(2)
  })
})

describe('TaskCard, working', () => {
  it('scans the live command under the summary and carries the crate mark', () => {
    renderCard({
      lifecycle: 'working',
      elapsed: '18:04',
      command: 'Reading internal/delegate/teardown.go',
      summary: 'Two readers hit the cgroup file at once.',
      href: '/runs/c1',
    })
    expect(screen.getByText('Reading internal/delegate/teardown.go')).toBeInTheDocument()
    expect(screen.getByText('Two readers hit the cgroup file at once.')).toBeInTheDocument()
    expect(document.querySelector('[data-mark="crates"] .tf-crates')).not.toBeNull()
    expect(screen.getByText('18:04')).toBeInTheDocument()
  })

  it('keeps the summary while the run has no command to report', () => {
    renderCard({
      lifecycle: 'working',
      elapsed: '0:12',
      summary: 'Two readers hit the cgroup file at once.',
      href: '/runs/c1',
    })
    expect(screen.getByText('Two readers hit the cgroup file at once.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'View run' })).toHaveAttribute('href', '/runs/c1')
  })

  it('lets a permission take the activity row, with the verbs, and keeps the summary', () => {
    const onAllow = vi.fn()
    const onDeny = vi.fn()
    renderCard({
      lifecycle: 'working',
      elapsed: '08:22',
      command: 'Reading a file',
      summary: 'Two readers hit the cgroup file at once.',
      href: '/runs/c1',
      permission: { command: 'psql -f migrations/0042_up.sql', count: 3, onAllow, onDeny },
    })
    expect(screen.getByText('psql -f migrations/0042_up.sql')).toBeInTheDocument()
    expect(screen.queryByText('Reading a file')).not.toBeInTheDocument()
    expect(screen.getByText('Two readers hit the cgroup file at once.')).toBeInTheDocument()
    // The stack depth rides inside the shield; there is no "+N more" row.
    expect(screen.getByText('3')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /Allow/ }))
    fireEvent.click(screen.getByRole('button', { name: /Deny/ }))
    expect(onAllow).toHaveBeenCalledTimes(1)
    expect(onDeny).toHaveBeenCalledTimes(1)
  })

  it('removes the verbs, not greys them, for a reader who may not answer', () => {
    renderCard({
      lifecycle: 'working',
      href: '/runs/c1',
      permission: { command: 'rm -rf build', count: 1 },
      interactive: false,
    })
    expect(screen.queryByRole('button', { name: /Allow/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Deny/ })).toBeNull()
  })
})

describe('TaskCard, settled', () => {
  it('folds the row into the footer with the ending on it', () => {
    renderCard({
      lifecycle: 'done',
      elapsed: '4:12',
      href: '/runs/c1',
      summary: 'Serialized the read behind a mutex.',
      artifacts: { branch: 1, comment: 2 },
    })
    expect(screen.getByRole('link', { name: 'Ran for 4:12' })).toHaveAttribute('href', '/runs/c1')
    expect(screen.getByText('Serialized the read behind a mutex.')).toBeInTheDocument()
    expect(document.querySelector('[data-mark="done"]')).not.toBeNull()
    expect(bracketWarm()).toContain('35%')
  })

  it('names each ending, and only failure spends a hue', () => {
    const { unmount } = renderCard({ lifecycle: 'failed', elapsed: '2:06', href: '/runs/c1' })
    expect(screen.getByRole('link', { name: 'Failed after 2:06' })).toBeInTheDocument()
    expect(document.querySelector('[data-mark="failed"]')).not.toBeNull()
    unmount()
    renderCard({ lifecycle: 'idle', elapsed: '6:20', href: '/runs/c1' })
    expect(screen.getByRole('link', { name: 'Idle after 6:20' })).toBeInTheDocument()
  })

  it('states the age of a task ended with no run to view', () => {
    renderCard({ lifecycle: 'done', age: '2d ago' })
    expect(screen.getByText('2d ago')).toBeInTheDocument()
    expect(screen.queryByText('View run')).not.toBeInTheDocument()
  })
})

describe('TaskCard, the task’s own asks', () => {
  it('names the memory wait and leads to the run page', () => {
    renderCard({ lifecycle: 'queued', age: '1h ago', waiting: true, href: '/runs/c0' })
    expect(screen.getByRole('link', { name: 'Waiting on memory' })).toHaveAttribute(
      'href',
      '/runs/c0',
    )
  })

  it('offers the retry for a delegation that never fired', () => {
    const onRetry = vi.fn()
    renderCard({
      lifecycle: 'queued',
      age: '1h ago',
      retry: { message: 'prompt not found', onRetry },
    })
    fireEvent.click(screen.getByRole('button', { name: /Retry/ }))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })
})
