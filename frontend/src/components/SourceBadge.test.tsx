import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import SourceBadge from './SourceBadge'
import type { Task } from '../types'

const task = (over: Partial<Task>): Task =>
  ({
    id: 't1',
    source: 'github',
    entity_kind: 'pr',
    ...over,
  }) as unknown as Task

describe('SourceBadge', () => {
  it('renders the Slack pill for a slack task', () => {
    render(<SourceBadge task={task({ source: 'slack', entity_kind: 'message' })} />)
    expect(screen.getByText('Slack')).toBeInTheDocument()
  })

  it('renders the Slack pill at lg size too', () => {
    render(<SourceBadge task={task({ source: 'slack', entity_kind: 'message' })} size="lg" />)
    expect(screen.getByText('Slack')).toBeInTheDocument()
  })

  it('leaves the github and jira badges unchanged', () => {
    const { rerender } = render(<SourceBadge task={task({ source: 'github' })} />)
    expect(screen.getByText('PR')).toBeInTheDocument()

    rerender(<SourceBadge task={task({ source: 'github', entity_kind: 'issue' })} />)
    expect(screen.getByText('GH')).toBeInTheDocument()

    rerender(<SourceBadge task={task({ source: 'jira', entity_kind: 'issue' })} />)
    expect(screen.getByText('Jira')).toBeInTheDocument()
  })
})
