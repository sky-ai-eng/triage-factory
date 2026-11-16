// @vitest-environment node
//
// Node, not the suite's default jsdom: the rule resolves the types.ts mirror
// through import.meta.url, which only carries a file: URL outside the browser
// environment.
import { fileURLToPath } from 'node:url'
import { RuleTester } from 'eslint'
import tseslint from 'typescript-eslint'
import { describe, expect, it } from 'vitest'
import rule, { CONVERSATION_STATUS_VOCABULARY } from './no-ghost-conversation-status.js'

// The guard's own fixtures. The invalid cases are the reintroduced ghosts —
// each one is a line that shipped in this codebase and survived a mirror test
// that could only see the arrays in types.ts. The valid cases are the two
// look-alike vocabularies (a task, a blueprint run) that legitimately
// terminate `cancelled`; a guard that fails on those is worse than none,
// because the fix would be to weaken it.
//
// RuleTester runs its assertions inline when no global describe/it exists
// (vitest runs with globals off), so each run() call throws inside its own it().
//
// The rule reads types, so each fixture is parsed as a file in TypeScript's
// default project, after a prelude that types the receivers the cases name: a
// conversation (`run`, `step`, `picked`) and the look-alikes beside it.
const fixtureDir = fileURLToPath(new URL('.', import.meta.url))
const ruleTester = new RuleTester({
  languageOptions: {
    parser: tseslint.parser,
    ecmaVersion: 2022,
    sourceType: 'module',
    parserOptions: {
      projectService: { allowDefaultProject: ['fixture.ts'] },
      tsconfigRootDir: fixtureDir,
    },
  },
})

const prelude = [
  'interface Conversation { status: string; next_attempt_at?: string }',
  'interface Task { status: string }',
  'interface BlueprintRun { status: string }',
  'interface Claim { status?: string; outcome: string }',
  'declare const run: Conversation',
  'declare const step: Conversation',
  "declare const picked: Pick<Conversation, 'status'>",
  'declare const task: Task',
  'declare const blueprintRun: BlueprintRun',
  'declare const either: Conversation | BlueprintRun',
  'declare const claim: Claim',
  'declare const request: { status: string }',
  'declare const thing: { status?: string }',
  '',
].join('\n')
const fixture = (test) => {
  const { code, ...rest } = typeof test === 'string' ? { code: test } : test
  return { ...rest, code: prelude + code, filename: `${fixtureDir}fixture.ts` }
}

// The message interpolates the whole vocabulary; taking it from the rule's own
// parse keeps a status added in Go from rewriting this file.
const vocabulary = [...CONVERSATION_STATUS_VOCABULARY].join(', ')
const ghost = (status) => ({ messageId: 'ghost', data: { status, vocabulary } })
const ghostAlias = (status, alias) => ({
  messageId: 'ghostAlias',
  data: { status, alias, vocabulary },
})

describe('no-ghost-conversation-status', () => {
  it('reads the vocabulary out of the types.ts mirror', () => {
    // The parse is the rule's whole world: everything it does not find here is
    // a ghost. Pin both ends — a real status it must know, and a retired one it
    // must not.
    expect([...CONVERSATION_STATUS_VOCABULARY].sort()).toEqual(
      [
        'agent_starting',
        'awaiting_credentials',
        'cloning',
        'failed',
        'fetching',
        'open',
        'queued',
        'running',
      ].sort(),
    )
  })

  it('passes real statuses, and both look-alike vocabularies', () => {
    ruleTester.run('no-ghost-conversation-status', rule, {
      valid: [
        // Every branch a conversation status may legitimately take.
        "if (run.status === 'open') park()",
        "if (run.status !== 'failed') wait()",
        "switch (run.status) { case 'queued': case 'awaiting_credentials': case 'failed': break }",
        // A status one hop from the property, named through the alias.
        "function tone(status: ConversationStatusValue) { return status === 'running' ? 'hot' : 'cold' }",
        "function tone({ status }: { status: ConversationStatusValue }) { switch (status) { case 'open': return 1 } }",
        // A TASK status — its own vocabulary, still terminates cancelled.
        "if (task.status === 'cancelled') archive()",
        // A blueprint RUN status — likewise, and the layer cancellation belongs to.
        "switch (blueprintRun.status) { case 'cancelled': return 'grey' }",
        // A union with another vocabulary: 'cancelled' is a real branch for
        // the blueprint run it may hold.
        "if (either.status === 'cancelled') archive()",
        // An unannotated local holding something status-shaped: out of scope by
        // construction, not by accident — nothing says it is a conversation.
        "const status = thing.status ?? 'idle'; if (status === 'cancelled') show()",
        // Claim OUTCOME, a third vocabulary that shares the word.
        "if (claim.outcome === 'cancelled') release()",
        // A real status behind a const alias — the alias is fine, the name is
        // what is checked.
        "const OPEN = 'open' as const; if (run.status === OPEN) park()",
        // A const alias compared against the OTHER vocabularies: following the
        // alias must not widen what the rule considers a conversation.
        "const CANCELLED = 'cancelled'; if (request.status === CANCELLED) show()",
        // Two statuses compared to each other name nothing at all.
        'if (run.status === step.status) same()',
      ].map(fixture),
      invalid: [],
    })
  })

  it('fails the build on every ghost the sweep removed', () => {
    ruleTester.run('no-ghost-conversation-status', rule, {
      valid: [],
      invalid: [
        {
          code: "if (run.status === 'cancelled') showCancelled()",
          errors: [ghost('cancelled')],
        },
        {
          code: "if (run.status === 'task_unsolvable' || run.status === 'pending_approval') nudge()",
          errors: [ghost('task_unsolvable'), ghost('pending_approval')],
        },
        {
          code: "switch (run.status) { case 'completed': return 1; case 'cancelled': return 2 }",
          errors: [ghost('completed'), ghost('cancelled')],
        },
        {
          // A conversation never concludes: a verdict parks it `open`.
          code: "if (run.status === 'completed') showDone()",
          errors: [ghost('completed')],
        },
        {
          code: "if ('worktree_created' === run.status) spin()",
          errors: [ghost('worktree_created')],
        },
        {
          code: "if (run?.status === 'initializing') spin()",
          errors: [ghost('initializing')],
        },
        // A Pick of the conversation is still its status.
        {
          code: "if (picked.status === 'cancelled') showCancelled()",
          errors: [ghost('cancelled')],
        },
        // Through the alias — the hop that hid two of the retired arms.
        {
          code: "function color(status: ConversationStatusValue) { switch (status) { case 'cancelled': return '#000' } }",
          errors: [ghost('cancelled')],
        },
        {
          code: "function tone({ status }: { status: ConversationStatusValue }) { return status === 'task_unsolvable' }",
          errors: [ghost('task_unsolvable')],
        },
        // Declared after its caller: the reason the checks run at Program:exit.
        {
          code: "const bad = tone('x'); function tone(status: ConversationStatusValue) { return status === 'cancelled' }",
          errors: [ghost('cancelled')],
        },
        // Behind a const alias — naming the ghost must not launder it.
        {
          code: "const CANCELLED = 'cancelled'; if (run.status === CANCELLED) showCancelled()",
          errors: [ghostAlias('cancelled', 'CANCELLED')],
        },
        {
          code: "const UNSOLVABLE = 'task_unsolvable' as const; switch (run.status) { case UNSOLVABLE: return 1 }",
          errors: [ghostAlias('task_unsolvable', 'UNSOLVABLE')],
        },
        {
          code: "const CANCELLED = 'cancelled'; function tone(status: ConversationStatusValue) { return status !== CANCELLED }",
          errors: [ghostAlias('cancelled', 'CANCELLED')],
        },
        // A declared status compared against a ghost: the declaration decides
        // which side is the subject, so its own initializer isn't mistaken for
        // the name under test.
        {
          code: "const status: ConversationStatusValue = 'open'; if (status === 'cancelled') showCancelled()",
          errors: [ghost('cancelled')],
        },
        // An `as ConversationStatusValue` cast names the value the same way.
        {
          code: "if (((claim.status ?? '') as ConversationStatusValue) === 'cancelled') show()",
          errors: [ghost('cancelled')],
        },
      ].map(fixture),
    })
  })
})
