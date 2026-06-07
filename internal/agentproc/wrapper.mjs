// Triage Factory Agent SDK shim. Spawned by internal/agentproc.Run as
// `node wrapper.mjs <flags>` in place of `claude -p ...`. Translates the
// flag-based argv that BuildArgs (argv.go) emits into Agent SDK Options
// and pipes the query() event stream to stdout in NDJSON, matching the
// `--output-format stream-json --verbose` shape the Go-side parser
// (stream.go) already consumes.
//
// Two modes, selected by argv:
//   - one-shot (default): a string `-p` prompt → query({prompt:string}).
//     Used by the scorer, classifier, profiler, curator, and
//     delegate-resume callers. Byte-for-byte the historical behavior.
//   - streaming-input (`--input-format stream-json`): the prompt is an
//     AsyncIterable<SDKUserMessage> fed from newline-delimited control
//     messages on stdin. This is the only mode in which the SDK exposes
//     its live controls — interrupt(), setPermissionMode(), and the
//     canUseTool permission callback — so it backs the interactive
//     LiveRun API on the Go side. The control protocol is documented
//     alongside the Go LiveRun type (live.go).
//
// Auth resolves through the SDK's own env-var/keychain logic — we
// deliberately don't touch ANTHROPIC_API_KEY, CLAUDE_CODE_USE_BEDROCK,
// AWS creds, or the macOS keychain entry here. Whatever the parent
// process (Triage Factory binary) put in env wins, and the SDK
// transparently falls back to the user's local Claude Code OAuth login
// when nothing is set, billing against their Pro/Max subscription.
import { query } from "@anthropic-ai/claude-agent-sdk"
import { createInterface } from "node:readline"

function parseArgs(argv) {
  let prompt = ""
  let streamInput = false
  const opts = {}
  const addDirs = []

  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i]
    const next = () => argv[++i]
    switch (flag) {
      case "-p":
        prompt = next()
        break
      case "--resume":
        opts.resume = next()
        break
      case "--model":
        opts.model = next()
        break
      case "--allowedTools":
        opts.allowedTools = next().split(",").filter(Boolean)
        break
      case "--add-dir":
        addDirs.push(next())
        break
      case "--append-system-prompt":
        // Preserve Claude Code's default system prompt and append the
        // runtime-specific text. Mirrors the CLI's --append-system-prompt
        // behavior; replacing the prompt entirely would clobber CC's
        // tool-use defaults.
        opts.systemPrompt = {
          type: "preset",
          preset: "claude_code",
          append: next(),
        }
        break
      case "--max-turns":
        opts.maxTurns = parseInt(next(), 10)
        break
      case "--input-format":
        // stream-json switches the wrapper into streaming-input mode:
        // the prompt becomes an AsyncIterable fed from stdin control
        // messages. Any other value is ignored (one-shot stays the
        // default).
        if (next() === "stream-json") streamInput = true
        break
      case "--output-format":
        // CLI flag — the SDK iterator already emits the same shape.
        next()
        break
      case "--verbose":
        // CLI flag — SDK is always verbose at this level.
        break
      default:
        process.stderr.write(`wrapper: ignoring unknown arg ${flag}\n`)
    }
  }

  if (addDirs.length > 0) opts.additionalDirectories = addDirs
  return { prompt, options: opts, streamInput }
}

function emit(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n")
}

// createInputStream returns an AsyncIterable the SDK consumes as the
// prompt, plus push()/end() controls the stdin reader drives. Messages
// pushed before the SDK pulls are buffered; end() lets the query drain
// and the process exit.
function createInputStream() {
  const queue = []
  let pendingResolve = null
  let ended = false

  return {
    push(msg) {
      if (ended) return
      if (pendingResolve) {
        const resolve = pendingResolve
        pendingResolve = null
        resolve({ value: msg, done: false })
      } else {
        queue.push(msg)
      }
    },
    end() {
      if (ended) return
      ended = true
      if (pendingResolve) {
        const resolve = pendingResolve
        pendingResolve = null
        resolve({ value: undefined, done: true })
      }
    },
    [Symbol.asyncIterator]() {
      return {
        next() {
          if (queue.length > 0) {
            return Promise.resolve({ value: queue.shift(), done: false })
          }
          if (ended) {
            return Promise.resolve({ value: undefined, done: true })
          }
          return new Promise((resolve) => {
            pendingResolve = resolve
          })
        },
      }
    },
  }
}

function userMessage(text) {
  return {
    type: "user",
    message: { role: "user", content: text ?? "" },
    parent_tool_use_id: null,
  }
}

// runStreamingInput drives the streaming-input mode: it wires the
// canUseTool permission round-trip, creates the streaming query, and
// dispatches stdin control messages until end/EOF.
async function runStreamingInput(options) {
  const input = createInputStream()
  const pendingPerms = new Map()
  let permCounter = 0

  // canUseTool is only honored in streaming-input mode. Each call emits
  // a permission_request and parks on a promise the matching
  // permission_response (from stdin) resolves. Returning the SDK's
  // PermissionResult (sdk.d.ts) shape: allow|deny.
  options.canUseTool = async (toolName, toolInput) => {
    const requestId = `perm-${++permCounter}`
    emit({
      type: "control",
      subtype: "permission_request",
      request_id: requestId,
      tool_name: toolName,
      input: toolInput,
    })
    return new Promise((resolve) => {
      pendingPerms.set(requestId, resolve)
    })
  }

  const q = query({ prompt: input, options })

  const rl = createInterface({ input: process.stdin })
  rl.on("line", (line) => {
    handleControl(line, q, input, pendingPerms)
  })
  // Stdin EOF ends the input iterable so the query drains and exits.
  rl.on("close", () => input.end())

  // Signal the Go side it can start sending user messages.
  emit({ type: "control", subtype: "ready" })

  try {
    for await (const msg of q) {
      process.stdout.write(JSON.stringify(msg) + "\n")
    }
  } catch (err) {
    process.stderr.write(`wrapper error: ${err?.stack ?? err}\n`)
    process.exit(1)
  }

  // The query drained (input ended) — exit promptly rather than letting
  // the still-open stdin readline keep the event loop alive.
  process.exit(0)
}

async function handleControl(line, q, input, pendingPerms) {
  const trimmed = line.trim()
  if (!trimmed) return

  let ctl
  try {
    ctl = JSON.parse(trimmed)
  } catch {
    process.stderr.write(`wrapper: bad control line: ${trimmed}\n`)
    return
  }

  switch (ctl.kind) {
    case "user_message":
      input.push(userMessage(ctl.text))
      break

    case "interrupt":
      try {
        await q.interrupt()
      } catch (err) {
        process.stderr.write(`wrapper: interrupt failed: ${err?.stack ?? err}\n`)
      }
      emit({ type: "control", subtype: "interrupted" })
      break

    case "set_mode":
      try {
        await q.setPermissionMode(ctl.mode)
      } catch (err) {
        process.stderr.write(`wrapper: set_mode failed: ${err?.stack ?? err}\n`)
      }
      break

    case "permission_response": {
      const resolve = pendingPerms.get(ctl.request_id)
      if (!resolve) {
        process.stderr.write(`wrapper: no pending permission ${ctl.request_id}\n`)
        break
      }
      pendingPerms.delete(ctl.request_id)
      if (ctl.behavior === "allow") {
        const result = { behavior: "allow" }
        if (ctl.updated_input) result.updatedInput = ctl.updated_input
        resolve(result)
      } else {
        resolve({ behavior: "deny", message: ctl.message ?? "denied by user" })
      }
      break
    }

    case "end":
      input.end()
      break

    default:
      process.stderr.write(`wrapper: unknown control kind ${ctl.kind}\n`)
  }
}

const { prompt, options, streamInput } = parseArgs(process.argv.slice(2))

// Forward the spawned Claude Code subprocess's stderr through our own.
// Without this, the SDK swallows the child's stderr ("ignore" in its
// stdio config) and a non-zero exit surfaces as the bare "Claude Code
// process exited with code N" — no auth error / arg parse error / etc.
// to act on. Piping it here lands the real message in agentproc.Run's
// stderrBuf and from there into the run's error log.
options.stderr = (chunk) => process.stderr.write(chunk)

if (streamInput) {
  await runStreamingInput(options)
} else {
  if (!prompt) {
    process.stderr.write("wrapper: missing -p <message>\n")
    process.exit(2)
  }

  try {
    for await (const msg of query({ prompt, options })) {
      process.stdout.write(JSON.stringify(msg) + "\n")
    }
  } catch (err) {
    process.stderr.write(`wrapper error: ${err?.stack ?? err}\n`)
    process.exit(1)
  }
}
