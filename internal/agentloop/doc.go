// Package agentloop is the native Go agent loop: the executor-side engine
// that drives a claimed `runtime='native'` conversation end to end without a
// JavaScript runtime inside the jail.
//
// One engagement is one call to Engine.Run. Per iteration, in order: drain
// the pending-input queue, check the guards, assemble the request from rows,
// resolve credentials, stream one completion, persist it as one row, dispatch
// its tool calls into the jail, and decide whether the turn concluded. The
// loop terminates when an assistant message arrives with no tool calls
// (implicit completion), when a flow-control tool sets an outcome, when a
// guard parks the conversation, or when the context is cancelled.
//
// Three properties the rest of the system depends on:
//
//   - Assembly is a pure function of rows. Everything that changes what the
//     model sees is a column on `messages`; this package holds no
//     conversation state between iterations that could alter it. Two
//     executors assembling the same rows build the same request.
//   - The transcript is legal and honest after any crash. Setup repairs it
//     unconditionally (see repair.go) rather than branching on whether this
//     is a resume, so there is one code path and it is the one that runs
//     every time.
//   - No tool call is ever executed twice. The crash window sits between
//     executing a tool and persisting its result, so a missing result is
//     repaired with a synthetic error, never a re-dispatch.
//
// The package is deliberately storage- and transport-agnostic at its edges:
// every dependency is a narrow interface (see engine.go), which is what lets
// the loop be driven end-to-end in tests against a fake provider and an
// in-memory transcript, with no database, jail, or network.
package agentloop
