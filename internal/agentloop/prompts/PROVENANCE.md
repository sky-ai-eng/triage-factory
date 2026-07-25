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

## `completion-native.txt`

TF-authored. The native path's terminal contract: implicit completion (an
assistant message with no tool calls) plus the `abort` flow-control tool. It
replaces the SDK path's JSON completion envelope
(`internal/ai/prompts/completion-sdk.txt`), which the native loop never parses.

## `blueprint-step-nonterminal.txt`

TF-authored, adapted from `internal/delegate/prompts/blueprint-step-nonterminal.txt`.
Same content and same guidance; the JSON-envelope references are rewritten as
the `continue` / `abort` tools, which is the only difference between the two
runtimes' non-terminal step instructions.

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
