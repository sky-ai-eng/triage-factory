import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SourceTag } from './cardChrome'
import type { Task } from '../../types'

const task = (over: Partial<Task>): Task =>
  ({
    id: 't1',
    source: 'github',
    entity_kind: 'pr',
    ...over,
  }) as unknown as Task

describe('SourceTag', () => {
  it('renders the SLACK glyph for a slack task', () => {
    render(<SourceTag task={task({ source: 'slack', entity_kind: 'message' })} />)
    expect(screen.getByText('SLACK')).toBeInTheDocument()
  })

  it('leaves the github and jira glyphs unchanged', () => {
    const { rerender } = render(<SourceTag task={task({ source: 'github' })} />)
    expect(screen.getByText('PR')).toBeInTheDocument()

    rerender(<SourceTag task={task({ source: 'github', entity_kind: 'issue' })} />)
    expect(screen.getByText('GH')).toBeInTheDocument()

    rerender(<SourceTag task={task({ source: 'jira', entity_kind: 'issue' })} />)
    expect(screen.getByText('JIRA')).toBeInTheDocument()
  })
})
