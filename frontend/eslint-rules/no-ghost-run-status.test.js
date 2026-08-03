// @vitest-environment node
//
// Node, not the suite's default jsdom: the rule resolves the types.ts mirror
// through import.meta.url, which only carries a file: URL outside the browser
// environment.
import { RuleTester } from 'eslint'
import tseslint from 'typescript-eslint'
import { describe, expect, it } from 'vitest'
import rule, { RUN_STATUS_VOCABULARY } from './no-ghost-run-status.js'

// The guard's own fixtures. The invalid cases are the reintroduced ghosts —
// each one is a line that shipped in this codebase and survived a mirror test
// that could only see the arrays in types.ts. The valid cases are the two
// look-alike vocabularies (a curator turn, a blueprint run) that legitimately
// terminate `cancelled`; a guard that fails on those is worse than none,
// because the fix would be to weaken it.
//
// RuleTester runs its assertions inline when no global describe/it exists
// (vitest runs with globals off), so each run() call throws inside its own it().
const ruleTester = new RuleTester({
  languageOptions: {
    parser: tseslint.parser,
    ecmaVersion: 2022,
    sourceType: 'module',
  },
})

// The message interpolates the whole vocabulary; taking it from the rule's own
// parse keeps a status added in Go from rewriting this file.
const vocabulary = [...RUN_STATUS_VOCABULARY].join(', ')
const ghost = (status) => ({ messageId: 'ghost', data: { status, vocabulary } })

describe('no-ghost-run-status', () => {
  it('reads the vocabulary out of the types.ts mirror', () => {
    // The parse is the rule's whole world: everything it does not find here is
    // a ghost. Pin both ends — a real status it must know, and a retired one it
    // must not.
    expect([...RUN_STATUS_VOCABULARY].sort()).toEqual(
      [
        'agent_starting',
        'awaiting_credentials',
        'cloning',
        'completed',
        'failed',
        'fetching',
        'open',
        'queued',
        'running',
      ].sort(),
    )
  })

  it('passes real statuses, and both look-alike vocabularies', () => {
    ruleTester.run('no-ghost-run-status', rule, {
      valid: [
        // Every branch a conversation status may legitimately take.
        "if (run.Status === 'open') park()",
        "if (run.Status !== 'completed') wait()",
        "switch (run.Status) { case 'queued': case 'awaiting_credentials': case 'failed': break }",
        // A status one hop from the property, named through the alias.
        "function tone(status: RunStatusValue) { return status === 'running' ? 'hot' : 'cold' }",
        "function tone({ status }: { status: RunStatusValue }) { switch (status) { case 'open': return 1 } }",
        // A curator TURN status — its own vocabulary, still terminates cancelled.
        "if (request.status === 'cancelled') show()",
        "function tone(status: CuratorRequestStatus) { return status === 'cancelled' }",
        // A blueprint RUN status — likewise, and the layer cancellation belongs to.
        "switch (blueprintRun.status) { case 'cancelled': return 'grey' }",
        // An unannotated local holding something status-shaped: out of scope by
        // construction, not by accident — nothing says it is a conversation.
        "const status = thing.status ?? 'idle'; if (status === 'cancelled') show()",
        // Claim OUTCOME, a third vocabulary that shares the word.
        "if (claim.outcome === 'cancelled') release()",
      ],
      invalid: [],
    })
  })

  it('fails the build on every ghost the sweep removed', () => {
    ruleTester.run('no-ghost-run-status', rule, {
      valid: [],
      invalid: [
        {
          code: "if (run.Status === 'cancelled') showCancelled()",
          errors: [ghost('cancelled')],
        },
        {
          code: "if (run.Status === 'task_unsolvable' || run.Status === 'pending_approval') nudge()",
          errors: [ghost('task_unsolvable'), ghost('pending_approval')],
        },
        {
          code: "switch (run.Status) { case 'completed': return 1; case 'cancelled': return 2 }",
          errors: [ghost('cancelled')],
        },
        {
          code: "if ('worktree_created' === run.Status) spin()",
          errors: [ghost('worktree_created')],
        },
        {
          code: "if (run?.Status === 'initializing') spin()",
          errors: [ghost('initializing')],
        },
        // Through the alias — the hop that hid two of the retired arms.
        {
          code: "function color(status: RunStatusValue) { switch (status) { case 'cancelled': return '#000' } }",
          errors: [ghost('cancelled')],
        },
        {
          code: "function tone({ status }: { status: RunStatusValue }) { return status === 'task_unsolvable' }",
          errors: [ghost('task_unsolvable')],
        },
        // Declared after its caller: the reason the checks run at Program:exit.
        {
          code: "const bad = tone('x'); function tone(status: RunStatusValue) { return status === 'cancelled' }",
          errors: [ghost('cancelled')],
        },
      ],
    })
  })
})
