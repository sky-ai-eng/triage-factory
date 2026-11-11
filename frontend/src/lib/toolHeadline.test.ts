import { describe, it, expect } from 'vitest'
import { bashHeadline, firstLine, isBashTool, toolHeadline } from './toolHeadline'

describe('isBashTool', () => {
  it('matches both runtimes spelling of the same tool', () => {
    // Native declares the tool `bash`; the Claude Code SDK declares `Bash`.
    // A helper that knew only one would leave the chain dead on the other.
    expect(isBashTool('bash')).toBe(true)
    expect(isBashTool('Bash')).toBe(true)
    expect(isBashTool('read')).toBe(false)
    expect(isBashTool('BashTool')).toBe(false)
  })
})

describe('bashHeadline', () => {
  const authored = {
    command: 'go test ./internal/sandbox -run TestSampler_Series -count=20',
    description: 'Reproducing the flake',
    description_past: 'Ran the sampler test 50x',
  }

  it('reads the present tense in flight and the past tense once it succeeds', () => {
    expect(bashHeadline(authored, 'running')).toBe('Reproducing the flake')
    expect(bashHeadline(authored, 'succeeded')).toBe('Ran the sampler test 50x')
  })

  it('reads the present tense on failure — a past tense beside a failure marker contradicts it', () => {
    expect(bashHeadline(authored, 'failed')).toBe('Reproducing the flake')
  })

  it('falls back to the present tense in every state when only it was authored', () => {
    // The SDK path is permanently here: it declares `description` and there is
    // no way to add a second param to a tool we do not define. The line must
    // read as a line, with no tell that a field was missing.
    const sdk = { command: authored.command, description: 'Reproducing the flake' }
    for (const state of ['running', 'succeeded', 'failed'] as const) {
      expect(bashHeadline(sdk, state)).toBe('Reproducing the flake')
    }
  })

  it('falls back to the command when the model authored nothing', () => {
    const bare = { command: authored.command }
    expect(bashHeadline(bare, 'running')).toBe(authored.command)
    expect(bashHeadline(bare, 'succeeded')).toBe(authored.command)
  })

  it('treats an empty or non-string summary as unauthored', () => {
    expect(bashHeadline({ command: 'ls', description: '' }, 'running')).toBe('ls')
    expect(bashHeadline({ command: 'ls', description: '   ' }, 'running')).toBe('ls')
    expect(bashHeadline({ command: 'ls', description: 42 }, 'running')).toBe('ls')
    // A settled call with only a blank past tense still has a present one.
    expect(
      bashHeadline(
        { command: 'ls', description: 'Listing the tree', description_past: '' },
        'succeeded',
      ),
    ).toBe('Listing the tree')
  })

  it('flattens an authored summary to one line without touching its voice', () => {
    // Whitespace is the renderer's business; tense and length are the author's.
    expect(bashHeadline({ command: 'ls', description: ' Run   the\ntests ' }, 'running')).toBe(
      'Run the tests',
    )
  })

  it('lets the surface render the last step its own way', () => {
    const shout = (cmd: string) => `$ ${cmd}`
    expect(bashHeadline({ command: 'ls' }, 'running', shout)).toBe('$ ls')
    // The authored summary still wins — the renderer is the fallback, not an override.
    expect(bashHeadline({ command: 'ls', description: 'Listing' }, 'running', shout)).toBe(
      'Listing',
    )
  })
})

describe('firstLine', () => {
  it('trims a single-line command and marks one that continues', () => {
    expect(firstLine('  go build ./...  ')).toBe('go build ./...')
    expect(firstLine("cat > f <<'EOF'\nbody\nEOF")).toBe("cat > f <<'EOF' …")
    expect(firstLine('')).toBe('')
  })
})

describe('toolHeadline', () => {
  // One row per tool, called the way each runtime calls it with the same
  // argument value. `sdk` is null where the SDK has no such tool.
  type Call = [name: string, input: Record<string, unknown>]
  const cases: { tool: string; native: Call; sdk: Call | null; want: string }[] = [
    {
      tool: 'read',
      native: ['read', { path: 'a.go' }],
      sdk: ['Read', { file_path: 'a.go' }],
      want: 'a.go',
    },
    { tool: 'read, no path', native: ['read', {}], sdk: ['Read', {}], want: '(no path)' },
    {
      tool: 'write',
      native: ['write', { path: 'a.go', content: 'package a' }],
      sdk: ['Write', { file_path: 'a.go', content: 'package a' }],
      want: 'a.go',
    },
    {
      // The model chooses the key order, and a short `content` first must
      // not take the headline from the path.
      tool: 'write, content first',
      native: ['write', { content: 'package a', path: 'a.go' }],
      sdk: ['Write', { content: 'package a', file_path: 'a.go' }],
      want: 'a.go',
    },
    {
      tool: 'write, no path',
      native: ['write', { content: 'x' }],
      sdk: ['Write', { content: 'x' }],
      want: '(no path)',
    },
    {
      tool: 'edit',
      native: ['edit', { path: 'a.go', edits: [] }],
      sdk: ['Edit', { file_path: 'a.go', old_string: 'x', new_string: 'y' }],
      want: 'a.go',
    },
    {
      tool: 'edit, no path',
      native: ['edit', { edits: [] }],
      sdk: ['Edit', {}],
      want: '(no path)',
    },
    {
      tool: 'grep',
      native: ['grep', { pattern: 'TODO', path: 'src' }],
      sdk: ['Grep', { pattern: 'TODO', path: 'src' }],
      want: 'TODO',
    },
    {
      tool: 'grep, no pattern',
      native: ['grep', { path: 'src' }],
      sdk: ['Grep', { path: 'src' }],
      want: '(no pattern)',
    },
    {
      tool: 'find',
      native: ['find', { pattern: '**/*.ts', path: 'src' }],
      sdk: ['Glob', { pattern: '**/*.ts', path: 'src' }],
      want: '**/*.ts',
    },
    {
      tool: 'find, no pattern',
      native: ['find', { path: 'src' }],
      sdk: ['Glob', { path: 'src' }],
      want: '(no pattern)',
    },
    { tool: 'ls', native: ['ls', { path: 'src' }], sdk: null, want: 'src' },
    { tool: 'ls, no path', native: ['ls', {}], sdk: null, want: '(no path)' },
    {
      tool: 'bash',
      native: ['bash', { command: 'go vet ./...', description: 'Vetting' }],
      sdk: ['Bash', { command: 'go vet ./...', description: 'Vetting' }],
      want: 'Vetting',
    },
    { tool: 'bash, no command', native: ['bash', {}], sdk: ['Bash', {}], want: '(no command)' },
  ]

  it.each(cases)('$tool heads alike under both runtimes', ({ native, sdk, want }) => {
    expect(toolHeadline(native[0], native[1], 'running')).toBe(want)
    if (sdk) expect(toolHeadline(sdk[0], sdk[1], 'running')).toBe(want)
  })
})
