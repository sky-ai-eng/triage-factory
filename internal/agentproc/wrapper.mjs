// Triage Factory Agent SDK shim. Spawned by internal/agentproc.Run as
// `node wrapper.mjs <flags>` in place of `claude -p ...`. Translates the
// flag-based argv that BuildArgs (argv.go) emits into Agent SDK Options
// and pipes the query() event stream to stdout in NDJSON, matching the
// `--output-format stream-json --verbose` shape the Go-side parser
// (stream.go) already consumes.
//
// Two modes, selected by argv:
//   - one-shot (default): a string `-p` prompt → query({prompt:string}).
//     Used by the scorer, classifier, profiler, and
//     delegate-resume callers. Byte-for-byte the historical behavior.
//   - streaming-input (`--input-format stream-json`): the prompt is an
//     AsyncIterable<SDKUserMessage> fed from newline-delimited control
//     messages on stdin. This is the only mode in which the SDK exposes
//     its live controls — interrupt(), setPermissionMode(), the
//     canUseTool permission callback and the PreToolUse and PostToolBatch
//     hooks — so it backs the interactive LiveRun API on the Go side. It is
//     also the only mode that serves TF's own tools (--tf-tools), whose calls
//     are answered on the Go side. The control protocol is documented
//     alongside the Go LiveRun type (live.go).
//
// Auth resolves through the SDK's own env-var/keychain logic — we
// deliberately don't touch ANTHROPIC_API_KEY, CLAUDE_CODE_USE_BEDROCK,
// AWS creds, or the macOS keychain entry here. Whatever the parent
// process (Triage Factory binary) put in env and --settings wins, and the
// SDK falls back to the user's local Claude Code OAuth login when nothing
// is set, billing against their Pro/Max subscription.
//
// That login also carries the claude.ai connectors its account has connected
// (Linear, Google Drive, ...), which Claude Code fetches from the account and
// connects as MCP servers whichever settings sources a spawn loads. Every
// spawn turns them off (disableClaudeAiConnectors below) unless the Go side
// passes --claude-ai-connectors.
import { query } from "@anthropic-ai/claude-agent-sdk"
import { randomUUID } from "node:crypto"
import { createInterface } from "node:readline"
import { createRequire } from "node:module"
import { existsSync, realpathSync, statSync } from "node:fs"
import { basename, dirname, join } from "node:path"

function resolveClaudeBinary() {
  if (process.platform !== "linux") return null
  const arch = process.arch
  let isMusl = false
  try {
    isMusl = !process.report?.getReport?.()?.header?.glibcVersionRuntime
  } catch {}
  const glibcPkg = `@anthropic-ai/claude-agent-sdk-linux-${arch}`
  const muslPkg = `${glibcPkg}-musl`
  const pkgs = isMusl ? [muslPkg, glibcPkg] : [glibcPkg, muslPkg]
  const require = createRequire(import.meta.url)
  for (const pkg of pkgs) {
    try {
      const bin = join(dirname(require.resolve(`${pkg}/package.json`)), "claude")
      if (existsSync(bin)) return bin
    } catch {}
  }
  return null
}

function parseArgs(argv) {
  let prompt = ""
  let streamInput = false
  let permissionPrompts = false
  let toolBatchNotes = false
  let claudeAiConnectors = false
  let tfTools = null
  const opts = {}
  const addDirs = []
  const pluginDirs = []
  const pluginRequired = []
  let pluginInitTimeoutMs = 0

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
      case "--permission-mode":
        opts.permissionMode = next()
        break
      case "--setting-sources":
        // An empty value becomes [], which loads no settings file at all;
        // leaving the option unset would load every source.
        opts.settingSources = next().split(",").filter(Boolean)
        break
      case "--settings":
        // A JSON object, handed to the SDK as one so it is validated here
        // rather than read by Claude Code as a file path.
        opts.settings = JSON.parse(next())
        break
      case "--claude-bin":
        // Explicit Claude binary (local-mode TF_CLAUDE_BINARY override). The Go
        // side validates the path and only passes this on the non-sandbox path,
        // so it wins over resolveClaudeBinary() below.
        opts.pathToClaudeCodeExecutable = next()
        break
      case "--allowedTools":
        opts.allowedTools = next().split(",").filter(Boolean)
        break
      case "--disallowedTools":
        // Bare names here are removed from the model's context outright,
        // so a tool nobody on the Go side can answer (AskUserQuestion) is
        // never offered rather than offered and denied.
        opts.disallowedTools = next().split(",").filter(Boolean)
        break
      case "--add-dir":
        addDirs.push(next())
        break
      case "--plugin-dir":
        // One of the team's local plugins, as the resolved directory Go
        // checked. Repeats, in the team's order; the order is what the
        // positional inline[N] tag on a load error counts.
        pluginDirs.push(next())
        break
      case "--plugin-required":
        // A --plugin-dir value, repeated, that must load before any tool
        // call is allowed (the plugin gate in runStreamingInput).
        pluginRequired.push(next())
        break
      case "--plugin-init-timeout-ms":
        pluginInitTimeoutMs = parseInt(next(), 10)
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
      case "--permission-prompts":
        // Opt into the canUseTool callback (valueless flag). Without it
        // we deliberately leave options.canUseTool unset so the SDK
        // gates purely on --allowedTools — off-allowlist tools auto-deny
        // with no prompt, matching the headless one-shot path. The Go
        // side (RunInteractive) emits this only when a permission
        // handler was supplied.
        permissionPrompts = true
        break
      case "--tool-batch-notes":
        // Opt into the PostToolBatch hook (valueless flag): after each
        // tool batch the wrapper asks the Go side for text to hand the
        // model before its next request. The Go side emits it only when
        // it has something to answer with. --tf-tools wires the same hook,
        // since Go answers there whether a batch ended the run.
        toolBatchNotes = true
        break
      case "--tf-tools":
        // The TF tools this run is offered, as [{name, description,
        // input_schema}] with each schema plain JSON Schema. Go holds their
        // only definition; streaming-input mode serves them and relays every
        // call back to Go.
        tfTools = JSON.parse(next())
        break
      case "--claude-ai-connectors":
        // Leave the login's claude.ai connectors connected (valueless flag).
        // The Go side emits it only for a spawn on the operator's own Claude
        // Code login whose team allows the connectors.
        claudeAiConnectors = true
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
  // skipMcpDiscovery on every entry: TF launches no MCP server a plugin
  // declares. It does not stop a plugin's hook code from opening connections.
  if (pluginDirs.length > 0) {
    opts.plugins = pluginDirs.map((path) => ({ type: "local", path, skipMcpDiscovery: true }))
  }
  const plugins = { dirs: pluginDirs, required: pluginRequired, initTimeoutMs: pluginInitTimeoutMs }
  // Set here rather than on the Go side so it covers every spawn the same way,
  // a run with a stored key included, whose argv carries no --settings at all.
  // Merged over --settings, which holds the operator's auth helpers. Left unset
  // rather than false when allowed: any settings source that sets it true
  // turns the connectors off, so a false here would override nothing.
  if (!claudeAiConnectors) {
    opts.settings = { ...opts.settings, disableClaudeAiConnectors: true }
  }
  return { prompt, options: opts, streamInput, permissionPrompts, toolBatchNotes, tfTools, plugins }
}

function emit(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n")
}

// emitThen writes one line and calls cb once the line has been handed to the
// OS. The process exits in cb on the plugin failure path, and an exit before
// the write lands would lose the one line that says why the run failed.
function emitThen(obj, cb) {
  let called = false
  const once = () => {
    if (called) return
    called = true
    cb()
  }
  process.stdout.write(JSON.stringify(obj) + "\n", once)
  return once
}

// pluginExitCode is the wrapper's exit status when a required plugin did not
// load. 1 is a query error and 2 a missing prompt.
const pluginExitCode = 3

// pluginExitFallbackMs bounds the wait for the failure line's write callback,
// which a stuck stdout would never call.
const pluginExitFallbackMs = 2000

// fileIdentity names the file at p by device and inode, read as bigints
// because an NTFS file id exceeds 2^53. null when p cannot be stat'ed, which
// matches nothing. Paths are never compared as strings: Claude Code reports a
// normalised form of the path it was given (case, separators, 8.3 names,
// /var against /private/var).
function fileIdentity(p) {
  if (typeof p !== "string" || p === "") return null
  try {
    const s = statSync(p, { bigint: true })
    return `${s.dev}:${s.ino}`
  } catch {
    return null
  }
}

function displayPath(p) {
  try {
    return realpathSync.native(p)
  } catch {
    return p
  }
}

// createPluginGate holds the plugin latch. With any required plugin it starts
// closed, and while closed every tool call is denied, main thread and
// subagent alike. Each system/init is evaluated: a required plugin passes
// only when it matched a loaded row by file identity and no load error is
// tied to it, and a failure on any evaluation closes the latch for good and
// ends the process. With no required plugin it is open from the start and
// only reports.
//
// The deadline starts at the first user message, because in streaming-input
// mode Claude Code emits init only after its first input arrives.
function createPluginGate({ dirs, required, initTimeoutMs }) {
  const requiredSet = new Set(required)
  const entries = dirs.map((path, index) => ({
    path,
    index,
    required: requiredSet.has(path),
    id: fileIdentity(path),
    dirNames: new Set([basename(path), basename(displayPath(path))].filter(Boolean)),
  }))
  const anyRequired = entries.some((e) => e.required)
  // "open" | "pending" | "failed"
  let state = anyRequired ? "pending" : "open"
  let sawInit = false
  let timer = null

  const fail = (failed) => {
    if (state === "failed") return
    state = "failed"
    if (timer) clearTimeout(timer)
    const exit = () => process.exit(pluginExitCode)
    emitThen({ type: "control", subtype: "plugins_loaded", ok: false, failed }, exit)
    setTimeout(exit, pluginExitFallbackMs)
  }

  return {
    active: entries.length > 0,

    // denial is the reason to deny a tool call, or null to let the call go
    // on to the rest of the checks.
    denial() {
      if (state === "pending") return "required plugin check pending"
      if (state === "failed") return "required plugin check failed"
      return null
    },

    armDeadline() {
      if (!anyRequired || timer || sawInit || state !== "pending") return
      const ms = initTimeoutMs > 0 ? initTimeoutMs : 60000
      timer = setTimeout(() => {
        if (sawInit) return
        fail(
          entries
            .filter((e) => e.required)
            .map((e) => ({
              path: displayPath(e.path),
              reason: `Claude Code did not report its loaded plugins within ${Math.round(ms / 1000)}s`,
              required: true,
            })),
        )
      }, ms)
    },

    onInit(init) {
      if (entries.length === 0 || state === "failed") return
      sawInit = true
      if (timer) {
        clearTimeout(timer)
        timer = null
      }
      const loaded = (Array.isArray(init?.plugins) ? init.plugins : []).map((row) => ({ row, id: fileIdentity(row?.path) }))
      const errors = (Array.isArray(init?.plugin_errors) ? init.plugin_errors : []).map((err) => ({
        err,
        id: fileIdentity(err?.path),
      }))
      const ok = []
      const failed = []
      // An error is tied to an entry when its tag names the plugin, by the
      // manifest name of the row it matched or by the entry's directory name
      // (Claude Code tags a component that failed to load, such as hooks.json,
      // with the directory's name: "<dir>@inline"), when its path is the
      // entry's file, or when it carries the entry's positional inline[N] tag.
      const tagNames = (tag) => {
        const at = tag.lastIndexOf("@")
        return at > 0 ? tag.slice(0, at) : tag
      }
      const isTiedTo = (entry, name, { err, id }) => {
        const tag = typeof err?.plugin === "string" ? err.plugin : ""
        const tagName = tagNames(tag)
        if (name !== "" && tagName === name) return true
        if (tagName !== "" && entry.dirNames.has(tagName)) return true
        if (entry.id && id === entry.id) return true
        return tag === `inline[${entry.index}]`
      }
      const matches = entries.map((entry) => {
        const match = entry.id ? loaded.find((l) => l.id === entry.id) : undefined
        const name = typeof match?.row?.name === "string" ? match.row.name : ""
        return { entry, match, name, tied: errors.filter((e) => isTiedTo(entry, name, e)) }
      })
      // Every directory this wrapper passes is an inline entry, so an inline
      // error that ties to none of them still belongs to one. It is tied to
      // every required entry rather than read as belonging to nobody.
      const unattributed = errors.filter(({ err }) => {
        const tag = typeof err?.plugin === "string" ? err.plugin : ""
        const inline = tag.endsWith("@inline") || tag.startsWith("inline[")
        return inline && !matches.some((m) => m.tied.some((t) => t.err === err))
      })
      for (const { entry, match, name, tied: own } of matches) {
        const tied = entry.required ? [...own, ...unattributed] : own
        const shown = displayPath(entry.path)
        if (match && tied.length === 0) {
          ok.push({ name, version: match.row.version, path: shown, required: entry.required })
          continue
        }
        let reason
        if (tied.length > 0) {
          reason = tied.map(({ err }) => `${err?.type ?? "error"}: ${err?.message ?? ""}`.trim()).join("; ")
        } else {
          reason = "Claude Code did not report it loaded"
        }
        failed.push({ path: shown, reason, required: entry.required })
      }
      if (failed.some((f) => f.required)) {
        fail(failed)
        return
      }
      if (state === "pending") state = "open"
      const report = { type: "control", subtype: "plugins_loaded", ok: true, plugins: ok }
      if (failed.length > 0) report.failed = failed
      emit(report)
    },
  }
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

// userMessage builds one SDKUserMessage. Its `message` is the API's
// MessageParam, whose content takes either a plain string or an array of
// content blocks — blocks when the Go side sent them, the string otherwise.
// Blocks win when both arrive: a turn assembled from several rows travels as
// those blocks, and joining them into a string would lose the boundaries.
function userMessage(text, blocks) {
  return {
    type: "user",
    message: { role: "user", content: blocks ?? (text ?? "") },
    parent_tool_use_id: null,
  }
}

// runStreamingInput drives the streaming-input mode: it (optionally) wires
// the canUseTool permission round-trip, creates the streaming query, and
// dispatches stdin control messages — one at a time, in arrival order —
// until end/EOF. permissionPrompts gates canUseTool: when false the
// callback is left unset and the SDK gates solely on --allowedTools, so an
// off-allowlist tool auto-denies with no prompt (the autonomous-run path).
// toolBatchNotes gates the PostToolBatch hook the same way, and tfTools, when
// present, the in-process MCP server that serves TF's tools.
async function runStreamingInput(options, permissionPrompts, toolBatchNotes, tfTools, plugins) {
  const input = createInputStream()
  const pluginGate = createPluginGate(plugins)
  const pendingPerms = new Map()
  const pendingBatches = new Map()
  const pendingToolCalls = new Map()
  const toolResult = (text, isError) => ({ content: [{ type: "text", text }], isError })
  let closing = false

  // Deny + clear every parked permission promise. Called on end/EOF (and
  // per-request on the abort signal) so the query can always drain
  // instead of hanging forever on a decision that will never arrive.
  const denyAllPending = (message) => {
    const pending = [...pendingPerms.values()]
    pendingPerms.clear()
    for (const { settle } of pending) settle({ behavior: "deny", message })
  }

  // canUseTool is only honored in streaming-input mode, and only wired
  // when the Go caller opted in (a permission handler was supplied). Each
  // call emits a permission_request and parks on a promise the matching
  // permission_response (from stdin) resolves. The settle wrapper is
  // idempotent (a one-shot guard) so an abort-signal denial, an explicit
  // response, and a close-time deny can't double-resolve. Once we're
  // closing, deny immediately so a late tool request can't re-block the
  // drain. Returns the SDK PermissionResult (sdk.d.ts) shape: allow|deny.
  //
  // When permissionPrompts is false we deliberately leave canUseTool
  // unset: the SDK then evaluates only the allow/deny rules from
  // --allowedTools, short-circuiting an allowlist match to allow and
  // auto-denying everything else — no callback, byte-identical to the
  // headless one-shot path.
  if (permissionPrompts) {
    options.canUseTool = async (toolName, toolInput, opts = {}) => {
      // The plugin latch first, as in onToolStarted.
      const latched = pluginGate.denial()
      if (latched) return { behavior: "deny", message: latched }
      if (closing) return { behavior: "deny", message: "run ending" }
      // toolUseID identifies the tool_use block being gated — the same id
      // that lands in the assistant message's tool_calls[] and on the tool
      // result that follows it. Passing it through verbatim is what lets a
      // decision be tied back to the call it authorized, and names the call
      // the way the native loop's own gate does.
      //
      // sdk.d.ts declares it non-optional, so the fallback below should be
      // unreachable. It exists because the failure mode without it is a
      // wedged run rather than a missing field: an absent id parks the
      // promise under the key `undefined`, JSON.stringify drops the key on
      // the way out, and the reply comes back carrying the empty string — a
      // key that matches nothing, so the promise never settles and the turn
      // hangs until the run ends. Parallel gated calls would also collapse
      // onto that one key and strand each other. A uuid keeps the prompt
      // answerable end to end; the prefix keeps it honest, since a synthetic
      // id matches nothing in the transcript.
      let toolCallID = opts.toolUseID
      if (typeof toolCallID !== "string" || toolCallID === "") {
        toolCallID = `synthetic-${randomUUID()}`
        process.stderr.write(
          `wrapper: permission request for ${toolName} carried no toolUseID; keyed as ${toolCallID}\n`,
        )
      }
      emit({
        type: "control",
        subtype: "permission_request",
        tool_call_id: toolCallID,
        tool_name: toolName,
        input: toolInput,
        // Prompt copy the SDK already rendered. Optional in sdk.d.ts, and
        // JSON.stringify drops undefined keys, so an absent field just means
        // the consumer falls back to reconstructing from tool_name + input.
        title: opts.title,
        display_name: opts.displayName,
        description: opts.description,
      })
      return new Promise((resolve) => {
        let done = false
        const settle = (decision) => {
          if (done) return
          done = true
          pendingPerms.delete(toolCallID)
          resolve(decision)
        }
        // Stash the original input alongside the settle: an allow
        // response that doesn't override the input echoes it back
        // as updatedInput (see the permission_response handler).
        pendingPerms.set(toolCallID, { settle, toolInput })
        // If the turn is interrupted, the SDK aborts this signal — resolve
        // as a deny so the pending promise never strands the query.
        const signal = opts.signal
        if (signal) {
          if (signal.aborted) {
            settle({ behavior: "deny", message: "interrupted" })
          } else {
            signal.addEventListener("abort", () => settle({ behavior: "deny", message: "interrupted" }), { once: true })
          }
        }
      })
    }
  }

  // PostToolBatch fires once every tool call of a batch has resolved, with
  // the model's next request waiting on it. Each one emits a tool_batch naming
  // the batch's calls and parks on the tool_batch_reply carrying the same id;
  // its context, when there is any, is handed to the model with the batch's
  // results, and its stop ends the turn here instead, which is how a TF tool
  // ends the run. Go decides stop, so the hook is wired whenever TF tools are
  // served, notes or not. Closing, an abort, and the drain all settle it with
  // nothing, so a reply that never comes cannot strand the turn. A subagent's
  // batch (agent_id set) asks nothing: its context would reach the subagent,
  // whose summary back to the agent need not carry it, and its calls cannot
  // end the agent's run.
  if (toolBatchNotes || tfTools) {
    const onToolBatch = async (input, _toolUseID, { signal } = {}) => {
      if (closing || input?.agent_id) return {}
      const id = randomUUID()
      // An id-less call is named as "", which Go never holds an ending for,
      // so the batch it is in cannot stop.
      const toolCallIDs = (input?.tool_calls ?? []).map((call) => call?.tool_use_id ?? "")
      emit({ type: "control", subtype: "tool_batch", id, tool_call_ids: toolCallIDs })
      const reply = await new Promise((resolve) => {
        let done = false
        const settle = (answer) => {
          if (done) return
          done = true
          pendingBatches.delete(id)
          resolve(answer)
        }
        pendingBatches.set(id, settle)
        if (signal) {
          if (signal.aborted) settle({})
          else signal.addEventListener("abort", () => settle({}), { once: true })
        }
      })
      if (reply.stop) return { continue: false, stopReason: "A Triage Factory tool ended the run." }
      if (!reply.context) return {}
      return { hookSpecificOutput: { hookEventName: "PostToolBatch", additionalContext: reply.context } }
    }
    options.hooks = { ...options.hooks, PostToolBatch: [{ hooks: [onToolBatch] }] }
  }

  // TF tools are served by an in-process MCP server named "tf", so the model
  // sees each one as mcp__tf__<name>. A call is relayed to Go as a tool_call
  // and parks on the tool_call_reply carrying the same id; Go runs the
  // handler. tool_call_id is the call's tool_use id, which the SDK hands the
  // handler in its _meta. An abort settles the call as an error and tells Go
  // to stop the handler; closing settles every call still waiting. zod is
  // imported only here, so a run offered no TF tool never loads it.
  if (tfTools) {
    const callTFTool = (name, args, extra) => {
      if (closing) return Promise.resolve(toolResult("run ended", true))
      const id = randomUUID()
      emit({
        type: "control",
        subtype: "tool_call",
        id,
        tool_call_id: extra?._meta?.["claudecode/toolUseId"],
        tool_name: name,
        input: args,
      })
      return new Promise((resolve) => {
        let done = false
        const settle = (result) => {
          if (done) return false
          done = true
          pendingToolCalls.delete(id)
          resolve(result)
          return true
        }
        pendingToolCalls.set(id, settle)
        const signal = extra?.signal
        if (signal) {
          const abort = () => {
            if (settle(toolResult("interrupted", true))) emit({ type: "control", subtype: "tool_call_cancel", id })
          }
          if (signal.aborted) abort()
          else signal.addEventListener("abort", abort, { once: true })
        }
      })
    }
    const [{ tool, createSdkMcpServer }, { fromJSONSchema }] = await Promise.all([
      import("@anthropic-ai/claude-agent-sdk"),
      import("zod"),
    ])
    const tools = tfTools.map((def) => {
      const shape = fromJSONSchema(def.input_schema).shape
      return tool(def.name, def.description, shape, (args, extra) => callTFTool(def.name, args, extra), {
        alwaysLoad: true,
      })
    })
    options.mcpServers = { ...options.mcpServers, tf: createSdkMcpServer({ name: "tf", alwaysLoad: true, tools }) }
  }

  // PreToolUse fires as Claude Code starts running a call, which for a call
  // that waited behind an earlier one in the same message is later than its
  // tool_use block. Each one emits a tool_started naming the call, so its
  // result is timed from when it ran. Nothing answers it and the hook never
  // waits: anything it held up would be the call it reports on. A subagent's
  // calls (agent_id set) are not reported, as in PostToolBatch.
  //
  // The same hook denies a subagent every TF tool. A subagent inherits its
  // parent's MCP servers, and Claude Code has no way to leave one tool out,
  // but a TF tool acts for the run itself: its handler is handed the
  // conversation's identity, and stop_blueprint ends the run only from a batch
  // of the main thread's.
  //
  // It is also the plugin latch's gate, because it is the one hook that sees
  // every call: canUseTool is wired only with --permission-prompts, and an
  // allowlisted call never reaches it. The latch test is the first statement,
  // ahead of the closing short-circuit, so ending the input cannot open it.
  const onToolStarted = async (input, toolUseID) => {
    const latched = pluginGate.denial()
    if (latched) {
      return {
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "deny",
          permissionDecisionReason: latched,
        },
      }
    }
    if (closing) return {}
    if (input?.agent_id) {
      if (typeof input.tool_name !== "string" || !input.tool_name.startsWith("mcp__tf__")) return {}
      return {
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "deny",
          permissionDecisionReason: "This tool is for the main agent only; a subagent cannot call it.",
        },
      }
    }
    const id = typeof toolUseID === "string" && toolUseID !== "" ? toolUseID : input?.tool_use_id
    if (typeof id === "string" && id !== "") {
      emit({ type: "control", subtype: "tool_started", tool_call_id: id })
    }
    return {}
  }
  options.hooks = { ...options.hooks, PreToolUse: [{ hooks: [onToolStarted] }] }

  const q = query({ prompt: input, options })

  // closeInput is the single drain path: deny anything parked, end the
  // input iterable, and latch `closing` so canUseTool auto-denies.
  // Idempotent — the end control and stdin EOF both route through it.
  const closeInput = () => {
    if (closing) return
    closing = true
    denyAllPending("run ended")
    for (const settle of [...pendingBatches.values()]) settle({})
    for (const settle of [...pendingToolCalls.values()]) settle(toolResult("run ended", true))
    input.end()
  }

  const handleControl = async (line) => {
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
        pluginGate.armDeadline()
        input.push(userMessage(ctl.text, ctl.blocks))
        break

      case "interrupt":
        try {
          await q.interrupt()
          // Only signal the interrupt once it actually landed — emitting
          // unconditionally would make the Go reader mislabel the next
          // result as error_during_execution even when interrupt threw.
          emit({ type: "control", subtype: "interrupted" })
        } catch (err) {
          process.stderr.write(`wrapper: interrupt failed: ${err?.stack ?? err}\n`)
        }
        break

      case "set_mode":
        try {
          await q.setPermissionMode(ctl.mode)
        } catch (err) {
          process.stderr.write(`wrapper: set_mode failed: ${err?.stack ?? err}\n`)
        }
        break

      case "permission_response": {
        const pending = pendingPerms.get(ctl.tool_call_id)
        if (!pending) {
          process.stderr.write(`wrapper: no pending permission ${ctl.tool_call_id}\n`)
          break
        }
        if (ctl.behavior === "allow") {
          // An allow always carries updatedInput: the Go side's override
          // when it sent one, the original input otherwise, so approve
          // means "run with the input you asked about". sdk.d.ts declares
          // the field optional and the bundled CLI falls back to the
          // original input on a bare allow; sending it keeps the decision
          // from depending on that fallback.
          pending.settle({ behavior: "allow", updatedInput: ctl.updated_input ?? pending.toolInput })
        } else {
          pending.settle({ behavior: "deny", message: ctl.message ?? "denied by user" })
        }
        break
      }

      case "tool_batch_reply": {
        // A reply with context says whether it reached the hook, so Go can
        // take back context that arrived after an interrupt gave the hook
        // up.
        const settle = pendingBatches.get(ctl.id)
        const context = ctl.context ?? ""
        if (settle) settle({ context, stop: Boolean(ctl.stop) })
        else process.stderr.write(`wrapper: no pending tool batch ${ctl.id}\n`)
        if (context) emit({ type: "control", subtype: "tool_batch_result", id: ctl.id, delivered: Boolean(settle) })
        break
      }

      case "tool_call_reply": {
        const settle = pendingToolCalls.get(ctl.id)
        if (settle) settle(toolResult(ctl.content ?? "", Boolean(ctl.is_error)))
        else process.stderr.write(`wrapper: no pending tool call ${ctl.id}\n`)
        break
      }

      case "end":
        closeInput()
        break

      default:
        process.stderr.write(`wrapper: unknown control kind ${ctl.kind}\n`)
    }
  }

  // Serialize control handling: chain each line behind the previous one
  // so interrupt/set_mode/end can't interleave or run out of order (a
  // bare rl.on("line") fires the async handlers concurrently).
  let controlChain = Promise.resolve()
  const rl = createInterface({ input: process.stdin })
  rl.on("line", (line) => {
    controlChain = controlChain
      .then(() => handleControl(line))
      .catch((err) => process.stderr.write(`wrapper: control handler error: ${err?.stack ?? err}\n`))
  })
  // Stdin EOF drains after any queued control lines so the query exits.
  rl.on("close", () => {
    controlChain = controlChain.then(() => closeInput())
  })

  // Signal the Go side it can start sending user messages.
  emit({ type: "control", subtype: "ready" })

  try {
    for await (const msg of q) {
      process.stdout.write(JSON.stringify(msg) + "\n")
      if (pluginGate.active && msg?.type === "system" && msg?.subtype === "init") pluginGate.onInit(msg)
    }
  } catch (err) {
    process.stderr.write(`wrapper error: ${err?.stack ?? err}\n`)
    process.exit(1)
  }

  // The query drained (input ended) — exit promptly rather than letting
  // the still-open stdin readline keep the event loop alive.
  process.exit(0)
}

const { prompt, options, streamInput, permissionPrompts, toolBatchNotes, tfTools, plugins } = parseArgs(process.argv.slice(2))

// Forward the spawned Claude Code subprocess's stderr through our own.
// Without this, the SDK swallows the child's stderr ("ignore" in its
// stdio config) and a non-zero exit surfaces as the bare "Claude Code
// process exited with code N" — no auth error / arg parse error / etc.
// to act on. Piping it here lands the real message in agentproc.Run's
// stderrBuf and from there into the run's error log.
options.stderr = (chunk) => process.stderr.write(chunk)

if (!options.pathToClaudeCodeExecutable) {
  const bin = resolveClaudeBinary()
  if (bin) options.pathToClaudeCodeExecutable = bin
}

if (streamInput) {
  await runStreamingInput(options, permissionPrompts, toolBatchNotes, tfTools, plugins)
} else {
  if (!prompt) {
    process.stderr.write("wrapper: missing -p <message>\n")
    process.exit(2)
  }
  // The plugin check lives in streaming-input mode. Go never sends plugins on
  // the one-shot path; refusing here keeps a required plugin from loading
  // unchecked if it ever did.
  if (plugins.dirs.length > 0) {
    process.stderr.write("wrapper: --plugin-dir needs --input-format stream-json\n")
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
