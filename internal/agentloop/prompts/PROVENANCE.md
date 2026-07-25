# Vendored prompt provenance

## `coding-agent-system.txt`

**Source: NOT YET VENDORED — stand-in text, must be replaced.**

The design of record calls for this file to carry pi's shipped coding-agent
system prompt *verbatim*, with its source commit recorded here, because that
prompt co-evolved with the seven tool definitions in `../tools/definitions.json`
(which *are* verbatim — see below). The text currently in the file is a
stand-in written against the same tool surface: it was authored here because
the pi tree was not reachable from the environment this landed in.

Replacing it is a one-file swap with no code change. When you do:

1. Copy the shipped prompt verbatim from
   `packages/coding-agent/src/core/prompts/` in the pi tree.
2. Record the commit here, e.g. `Source: pi @ <sha>, packages/coding-agent/src/core/prompts/<file>`.
3. Delete this section's warning.

Prompt composition and quality iteration are human-owned and deliberately out
of scope for the code that assembles them; see `../envelope.go` for what is
mechanical.

## `completion-blueprint.txt`

TF-authored. The native path's terminal contract: implicit completion (an
assistant message with no tool calls concludes the run and its text is the
summary), the `stop_blueprint` tool, and the artifact contract. It replaces
the SDK path's JSON completion envelope
(`internal/ai/prompts/completion-sdk.txt`), which the native loop never parses.

Appended only when the conversation executes a blueprint, gated by
`EnvelopeParts.HasBlueprint` — which must agree with `Spec.HasBlueprint`,
since that is what registers the tool this text describes.

There is no taskless counterpart, and adding one would be a mistake. Every
line here presupposes an absent human: a run that was dispatched, a task to
leave open, a mission that expected an artifact. A conversation with a person
in it ends when they stop writing, which is not a protocol and does not need
stating.

## `blueprint-step-nonterminal.txt`

TF-authored, adapted from `internal/delegate/prompts/blueprint-step-nonterminal.txt`.

The guidance on handoff and external actions is the same, but the two runtimes
differ on what stopping means, so this file is not a mechanical rewrite of the
SDK one. On the SDK path a step declares `continue` in its JSON envelope and
stopping any other way ends the blueprint; here stopping IS the handoff, and
ending the blueprint early is what takes a deliberate call. Edit them
independently.

## `../tools/definitions.json`

Generated verbatim by `tf-harness-tools --definitions` from
`harness/tf-harness-tools`, whose `src/definitions.rs` states it matches pi's
TypeBox output for the seven tools. Regenerate with:

```
(cd harness/tf-harness-tools && cargo run --quiet --bin tf-harness-tools -- --definitions) \
  > internal/agentloop/tools/definitions.json
```

`TestToolDefinitions_MatchHarness` regenerates and compares when cargo is
available, and skips when it is not.
