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

// A shell command is the only tool argument that says nothing to a person, so
// bash rows read their authored summary instead — in the tense the row's own
// state calls for. The tool arrives under two spellings and the summary only
// exists for the lowercase one, which is what makes the name normalization
// load-bearing rather than tidy.
describe('ScreenTranscript bash headlines', () => {
  const command = 'go test ./internal/sandbox -run TestSampler_Series -count=20'
  const call = (input: Record<string, unknown>, name = 'bash') =>
    msg({
      role: 'assistant',
      subtype: 'tool_use',
      tool_calls: [{ id: 'tc-1', name, input }],
    })
  const result = (over: Partial<Message> = {}) =>
    msg({ role: 'tool', tool_call_id: 'tc-1', content: 'ok', ...over })

  const authored = { command, description: 'Reproducing the flake', description_past: 'Ran it 50x' }

  it('shows the present tense while the call is in flight', () => {
    render(<ScreenTranscript conversation={conversation()} messages={[call(authored)]} />)
    expect(screen.getByText('Reproducing the flake')).toBeInTheDocument()
    expect(screen.queryByText(command)).not.toBeInTheDocument()
  })

  it('shows the past tense once the call settles successfully', () => {
    render(<ScreenTranscript conversation={conversation()} messages={[call(authored), result()]} />)
    expect(screen.getByText('Ran it 50x')).toBeInTheDocument()
  })

  it('goes back to the present tense when the call failed', () => {
    render(
      <ScreenTranscript
        conversation={conversation()}
        messages={[call(authored), result({ is_error: true, content: 'boom' })]}
      />,
    )
    expect(screen.getByText('Reproducing the flake')).toBeInTheDocument()
    expect(screen.queryByText('Ran it 50x')).not.toBeInTheDocument()
  })

  it('renders a present-tense-only row identically in every state', () => {
    // The SDK path, permanently: it declares `description` and nothing else.
    const sdk = { command, description: 'Reproducing the flake' }
    for (const rows of [[call(sdk, 'Bash')], [call(sdk, 'Bash'), result()]]) {
      const { unmount } = render(<ScreenTranscript conversation={conversation()} messages={rows} />)
      expect(screen.getByText('Reproducing the flake')).toBeInTheDocument()
      unmount()
    }
  })

  it('renders the command itself when the model authored no summary', () => {
    render(<ScreenTranscript conversation={conversation()} messages={[call({ command })]} />)
    expect(screen.getByText(command)).toBeInTheDocument()
  })
})
