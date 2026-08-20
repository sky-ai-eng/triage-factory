import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import ScreenTranscript from './ScreenTranscript'
import type { Conversation, Message } from '../../types'

const conversation = (over: Partial<Conversation> = {}): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'open',
    Model: 'claude-opus-4-8',
    StartedAt: '2026-06-25T00:00:00Z',
    ResultSummary: '',
    ...over,
  }) as Conversation

let nextID = 1
const msg = (over: Partial<Message>): Message => ({
  id: nextID++,
  conversation_id: 'r1',
  role: 'user',
  content: '',
  subtype: '',
  created_at: '2026-06-25T00:00:01Z',
  ...over,
})

// A role=user row can be a person or the machine, and the subtype is the only
// thing that says which. Rendering the machine's half as operator input puts
// words in the reader's mouth — at exactly the moments the machine is
// explaining why it stopped.
describe('ScreenTranscript system-authored rows', () => {
  it('renders a stop note as a marker, never as operator speech', () => {
    render(
      <ScreenTranscript
        conversation={conversation()}
        messages={[
          msg({ content: 'fix the failing build' }),
          msg({
            role: 'user',
            subtype: 'stop-note',
            content: 'Run stopped by the user. It may be resumed later.',
          }),
        ]}
      />,
    )
    expect(
      screen.getByText('Run stopped by the user. It may be resumed later.'),
    ).toBeInTheDocument()
    // Exactly one YOU tag: the operator's own line, not the stop note.
    expect(screen.getAllByText('you')).toHaveLength(1)
  })

  it('renders the engine-written park notice the same way', () => {
    render(
      <ScreenTranscript
        conversation={conversation()}
        messages={[
          msg({
            role: 'user',
            subtype: 'stop-note',
            content: 'This run reached its spend cap and has been paused.',
          }),
        ]}
      />,
    )
    expect(screen.queryByText('you')).not.toBeInTheDocument()
  })

  it('still renders a mid-work steer as operator speech', () => {
    render(
      <ScreenTranscript
        conversation={conversation()}
        messages={[
          msg({ role: 'user', subtype: 'injection:steer', content: 'also check the tests' }),
        ]}
      />,
    )
    expect(screen.getByText('you')).toBeInTheDocument()
    expect(screen.getByText('also check the tests')).toBeInTheDocument()
  })
})

// A stop concludes nothing, so the backend leaves the summary empty and the
// verdict block has nothing to render. This is the display half of that
// contract.
describe('ScreenTranscript verdict', () => {
  it('renders no verdict for a settled conversation with no summary', () => {
    render(<ScreenTranscript conversation={conversation({ Status: 'open' })} messages={[]} />)
    expect(screen.queryByText('IDLE')).not.toBeInTheDocument()
  })

  it('renders the verdict when a conversation actually concluded with one', () => {
    render(
      <ScreenTranscript
        conversation={conversation({
          Status: 'completed',
          Outcome: 'finish',
          ResultSummary: 'Opened a PR.',
        })}
        messages={[]}
      />,
    )
    expect(screen.getByText('Opened a PR.')).toBeInTheDocument()
  })
})
