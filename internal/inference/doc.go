// Package inference is the LLM layer of the native agent loop: a small
// TF-owned surface over embedded bifrost core.
//
// It owns three things and nothing else:
//
//   - The assembly bijection between the domain message rows a conversation
//     stores and the bifrost ChatMessage list a provider call sends
//     (RowsToMessages / MessageToRow). Assembly is a pure function of rows;
//     it never reaches back to the database or any process state.
//   - A streaming client (Stream) that reassembles a provider's delta stream
//     into one neutral assistant message per API message — reasoning details
//     with signatures intact, tool calls, and usage including cache tokens.
//   - Usage→dollars pricing (CostForUsage) computed from a pinned, vendored
//     snapshot of bifrost's model-pricing datasheet
//
// It deliberately holds no loop, no dispatcher wiring, and no
// credential-sealing integration: callers pass resolved credentials as plain
// inputs. bifrost is transport, not policy — this package does not build on
// its agent loop, MCP manager, or compaction request.
package inference
