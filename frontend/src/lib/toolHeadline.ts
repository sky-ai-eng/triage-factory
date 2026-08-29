// toolHeadline — the sentence a bash row shows in place of its command.
//
// A shell command is the only tool argument that is not already legible to a
// person: a path, a pattern, a directory each describe themselves, while
// `go test ./internal/sandbox -run TestSampler_Series -count=20` describes
// nothing. So the bash tool takes two authored summaries alongside the
// command — a present tense and a past tense — and this module is the single
// place that picks between them. The choice is a chain rather than a field
// read, and a surface that reimplemented it would drift.
//
// Two spellings arrive here and both are real: a native conversation calls
// the tool `bash`, the Claude Code SDK calls it `Bash`. Both carry
// `description` — the SDK declares that param itself, so the chain's first
// step works on either runtime. `description_past` is native-only and
// permanently so: local mode passes the SDK its tool names, not their
// schemas, so there is no way to add a param to a tool we do not define. An
// SDK row that settles keeps whatever voice the model chose. That is the end
// state, not a gap.

/** Where a bash call stands, which is what decides the tense. */
export type BashCallState = 'running' | 'succeeded' | 'failed'

/** True for a bash tool call under either runtime's spelling. */
export function isBashTool(name: string): boolean {
  return name === 'bash' || name === 'Bash'
}

/**
 * The headline for one bash call.
 *
 * In flight and failed both read `description`: a past tense sitting next to
 * a red failure marker reads as a contradiction, so the failed row stays in
 * the voice of something being attempted. A settled success prefers
 * `description_past`, falls back to `description`, and falls back again to
 * the command itself.
 *
 * `renderCommand` is that last step — how this surface likes to print a raw
 * command. It gets one, deliberately: a command shown here is a command
 * shown on purpose, never a label that failed to resolve.
 */
export function bashHeadline(
  input: Record<string, unknown>,
  state: BashCallState,
  renderCommand: (command: string) => string = firstLine,
): string {
  const present = summary(input.description)
  const past = state === 'succeeded' ? summary(input.description_past) : ''
  return past || present || renderCommand(String(input.command ?? ''))
}

/** The first line of a command, trimmed, marked when more lines follow. */
export function firstLine(command: string): string {
  const lines = command.split('\n')
  const head = lines[0].trim()
  if (!head) return ''
  return lines.length > 1 ? `${head} …` : head
}

// summary normalizes one authored field to a single line. Whitespace only —
// tense, voice and length belong to the author, and nothing here rewrites
// them.
function summary(value: unknown): string {
  if (typeof value !== 'string') return ''
  return value.replace(/\s+/g, ' ').trim()
}
