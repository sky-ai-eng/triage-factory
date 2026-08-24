// CLI verbs for `triagefactory exec slack ...` (TFAC-596) — the sandbox
// side. Registered via exec.RegisterSubcommand from init() (this is arg
// parsing only; every verb makes exactly one host.CallExtension("slack", ...)
// call and prints its JSON result verbatim — no Slack logic runs here, see
// docs/ee-feature-packaging.md's "Agent-facing CLI verbs" section). Verb-first
// grammar, no implicit targets anywhere: the conversation's prompt supplies the
// triggering channel/thread-ts (via {{EVENT_METADATA_JSON}}), and the agent
// passes them back explicitly on every call.
package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

func init() {
	exec.RegisterSubcommand("slack", exec.Subcommand{
		Run:        runSlackCLI,
		HelpText:   cliHelpText,
		ValueFlags: cliValueFlags,
		SourceKind: "slack",
	})
}

// cliHelpText is the `exec slack` section of the CLI help — the top-level
// index and every `slack ... --help` depth print from it. It is the
// CLI-format counterpart of prompts/slack.txt, which is prompt text (with
// placeholders and run-context rules that mean nothing in a terminal), never
// reused here. The built-ins' HelpText conventions apply: verbs named bare,
// the invocation prefix appears in the usage line alone.
const cliHelpText = `Slack Commands:
  slack send --channel <C> [--thread-ts <TS>] [--body <md> | --body-file <path|->] [--attach-file <path|->]
                                                          Post a message (Markdown). Needs a body
                                                          and/or an attach-file — a file-only send
                                                          is fine. Returns {"channel":..., "ts":...}.
  slack edit --channel <C> --ts <TS> (--body <md> | --body-file <path|->)
                                                          Edit a message the bot itself posted.
  slack react --channel <C> --ts <TS> --emoji <name>      Add an emoji reaction (name only, no
                                                          colons: thumbsup, not :thumbsup:).
  slack read thread --channel <C> --ts <TS> [--limit <N>] Read a thread's replies, oldest first.
  slack read channel --channel <C> (--limit <N> | --ts <TS> [--num-prior <N>] [--num-following <N>])
                                                          Read channel history: the latest N
                                                          messages, or a window around an anchor ts.
                                                          The two forms are mutually exclusive.
  slack download --id <file_id> [--out <path>]            Download a file from a read result's
                                                          files array; prints {"path":...}.

  --body-file and --attach-file accept "-" for stdin. Both reads return a JSON array of
  {ts, thread_ts, sender_id, sender_name, text, files:[{id,name}]} per message.`

// cliValueFlags is every exec-slack flag that takes a value, for the shared
// help scan (execflags.HasHelpFlag) — what keeps --body "--help" a message
// body rather than a help request. slack has no boolean flags.
var cliValueFlags = map[string]bool{
	"--channel":       true,
	"--ts":            true,
	"--thread-ts":     true,
	"--emoji":         true,
	"--body":          true,
	"--body-file":     true,
	"--attach-file":   true,
	"--limit":         true,
	"--num-prior":     true,
	"--num-following": true,
	"--id":            true,
	"--out":           true,
}

// slackVerbHelp maps each slack verb to its usage line(s), powering
// `slack <verb> --help` (cmd/exec/jira's ticketHelp is the pattern) — one
// line per form, so `read` carries both of its shapes. Mirrored in
// cliHelpText, the family overview — a rename has to touch both.
var slackVerbHelp = map[string][]string{
	"send":     {"slack send --channel <C> [--thread-ts <TS>] [--body <md> | --body-file <path|->] [--attach-file <path|->]"},
	"edit":     {"slack edit --channel <C> --ts <TS> (--body <md> | --body-file <path|->)"},
	"react":    {"slack react --channel <C> --ts <TS> --emoji <name>"},
	"read":     {"slack read thread --channel <C> --ts <TS> [--limit <N>]", "slack read channel --channel <C> (--limit <N> | --ts <TS> [--num-prior <N>] [--num-following <N>])"},
	"download": {"slack download --id <file_id> [--out <path>]"},
}

// runSlackCLI dispatches `exec slack <verb> ...`. Returns a process exit
// code, mirroring every other SubcommandRunner. host is nil on help routes —
// the exec dispatcher short-circuits them before building the agenthost — so
// every help path below returns before touching it.
func runSlackCLI(ctx context.Context, args []string, host agenthost.Client) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printSlackHelp()
		return 0
	}
	verb, rest := args[0], args[1:]

	// Per-verb --help: print just that verb's usage and exit cleanly, without
	// running the verb body (which would otherwise report its required flags
	// as missing). An unknown verb with --help falls through to the
	// unknown-verb error, so the caller learns the verb itself is wrong.
	if execflags.HasHelpFlag(rest, cliValueFlags) {
		if lines, ok := slackVerbHelp[verb]; ok {
			for _, h := range lines {
				fmt.Printf("usage: %s %s\n", prog.Prefix(), h)
			}
			return 0
		}
	}

	switch verb {
	case "send":
		return slackCLISend(ctx, rest, host)
	case "edit":
		return slackCLIEdit(ctx, rest, host)
	case "react":
		return slackCLIReact(ctx, rest, host)
	case "read":
		return slackCLIRead(ctx, rest, host)
	case "download":
		return slackCLIDownload(ctx, rest, host)
	default:
		slackReportErr(fmt.Sprintf("unknown slack verb: %s\nvalid verbs: download, edit, react, read, send\nRun '%s slack --help' for usage.", verb, prog.Prefix()))
		return 1
	}
}

func printSlackHelp() {
	fmt.Printf("Usage: %s slack <verb> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", prog.Prefix(), cliHelpText)
}

// --- shared arg-parsing / output helpers (mirrors cmd/exec/gh's idioms) ---

func slackFlagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func slackIntFlag(args []string, flag string) (int, error) {
	v := slackFlagVal(args, flag)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", flag, err)
	}
	return n, nil
}

func slackPrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func slackReportErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
}

func slackReportOnErr(err error) bool {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return true
	}
	return false
}

// slackReadBody resolves the --body/--body-file mutually-exclusive pair
// (cmd/exec/gh/gh.go's documented convention): "-" for --body-file means
// stdin. Empty/empty is valid (send allows a file-only message); both set is
// an error.
func slackReadBody(args []string) (string, error) {
	body := slackFlagVal(args, "--body")
	bodyFile := slackFlagVal(args, "--body-file")
	if body != "" && bodyFile != "" {
		return "", fmt.Errorf("--body and --body-file are mutually exclusive; pass one or the other")
	}
	if bodyFile == "" {
		return body, nil
	}
	var data []byte
	var err error
	if bodyFile == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(bodyFile)
	}
	if err != nil {
		return "", fmt.Errorf("read --body-file: %w", err)
	}
	return string(data), nil
}

// slackCallExtension marshals args, calls host.CallExtension("slack",
// method, ...), and unmarshals the result into out — the boilerplate every
// verb function shares.
func slackCallExtension(ctx context.Context, host agenthost.Client, method string, args, out any) error {
	payload, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	result, err := host.CallExtension(ctx, "slack", method, payload)
	if err != nil {
		return err
	}
	if out == nil || len(result) == 0 {
		return nil
	}
	return json.Unmarshal(result, out)
}

// --- send ---

func slackCLISend(ctx context.Context, args []string, host agenthost.Client) int {
	channel := slackFlagVal(args, "--channel")
	if channel == "" {
		slackReportErr("--channel is required")
		return 1
	}
	body, err := slackReadBody(args)
	if slackReportOnErr(err) {
		return 1
	}
	attachName, attachB64, err := slackReadAttachFile(args)
	if slackReportOnErr(err) {
		return 1
	}
	if body == "" && attachB64 == "" {
		slackReportErr("send needs --body/--body-file or --attach-file")
		return 1
	}

	a := slackSendArgs{
		Channel: channel, ThreadTS: slackFlagVal(args, "--thread-ts"),
		Body: body, AttachName: attachName, AttachBase64: attachB64,
	}
	var out slackSendResult
	if slackReportOnErr(slackCallExtension(ctx, host, "send", a, &out)) {
		return 1
	}
	slackPrintJSON(out)
	return 0
}

// slackReadAttachFile reads --attach-file's bytes (path, or "-" for stdin,
// matching --body-file's convention) and returns its base64 encoding plus
// its base name for the wire args. ("", "", nil) when --attach-file isn't
// passed. Enforces slackExecMaxFileBytes here, before base64-encoding and
// marshaling into the extension call's args: skipping this check would let
// an oversized file blow through the ~16 MiB IPC frame cap (base64 inflates
// it further) before the host-side guard in exec_host.go ever gets a chance
// to reject it with a clean error.
func slackReadAttachFile(args []string) (name, base64Body string, err error) {
	path := slackFlagVal(args, "--attach-file")
	if path == "" {
		return "", "", nil
	}
	var data []byte
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, slackExecMaxFileBytes+1))
		name = "attachment"
	} else {
		// Stat first so an oversized file on disk is rejected without
		// reading the whole thing into memory first.
		if fi, statErr := os.Stat(path); statErr == nil && fi.Size() > slackExecMaxFileBytes {
			return "", "", fmt.Errorf("--attach-file is %d bytes, exceeds the %d-byte cap", fi.Size(), slackExecMaxFileBytes)
		}
		data, err = os.ReadFile(path)
		name = baseName(path)
	}
	if err != nil {
		return "", "", fmt.Errorf("read --attach-file: %w", err)
	}
	if len(data) > slackExecMaxFileBytes {
		return "", "", fmt.Errorf("--attach-file is %d bytes, exceeds the %d-byte cap", len(data), slackExecMaxFileBytes)
	}
	return name, base64.StdEncoding.EncodeToString(data), nil
}

// baseName returns path's final path element without pulling in the full
// path/filepath dependency graph for one call site.
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}

// --- edit ---

func slackCLIEdit(ctx context.Context, args []string, host agenthost.Client) int {
	channel := slackFlagVal(args, "--channel")
	ts := slackFlagVal(args, "--ts")
	if channel == "" || ts == "" {
		slackReportErr("--channel and --ts are required")
		return 1
	}
	body, err := slackReadBody(args)
	if slackReportOnErr(err) {
		return 1
	}
	if body == "" {
		slackReportErr("--body or --body-file is required")
		return 1
	}

	a := slackEditArgs{Channel: channel, TS: ts, Body: body}
	var out slackEditResult
	if slackReportOnErr(slackCallExtension(ctx, host, "edit", a, &out)) {
		return 1
	}
	slackPrintJSON(out)
	return 0
}

// --- react ---

func slackCLIReact(ctx context.Context, args []string, host agenthost.Client) int {
	channel := slackFlagVal(args, "--channel")
	ts := slackFlagVal(args, "--ts")
	emoji := slackFlagVal(args, "--emoji")
	if channel == "" || ts == "" || emoji == "" {
		slackReportErr("--channel, --ts, and --emoji are required")
		return 1
	}

	a := slackReactArgs{Channel: channel, TS: ts, Emoji: emoji}
	var out slackReactResult
	if slackReportOnErr(slackCallExtension(ctx, host, "react", a, &out)) {
		return 1
	}
	slackPrintJSON(out)
	return 0
}

// --- read thread / read channel ---

func slackCLIRead(ctx context.Context, args []string, host agenthost.Client) int {
	if len(args) == 0 {
		slackReportErr(fmt.Sprintf("usage: %s slack read <thread|channel> ...", prog.Prefix()))
		return 1
	}
	mode, rest := args[0], args[1:]
	switch mode {
	case "thread":
		return slackCLIReadThread(ctx, rest, host)
	case "channel":
		return slackCLIReadChannel(ctx, rest, host)
	default:
		slackReportErr(fmt.Sprintf("unknown slack read mode: %s\nvalid modes: thread, channel\nRun '%s slack read --help' for usage.", mode, prog.Prefix()))
		return 1
	}
}

func slackCLIReadThread(ctx context.Context, args []string, host agenthost.Client) int {
	channel := slackFlagVal(args, "--channel")
	ts := slackFlagVal(args, "--ts")
	if channel == "" || ts == "" {
		slackReportErr("--channel and --ts are required")
		return 1
	}
	limit, err := slackIntFlag(args, "--limit")
	if slackReportOnErr(err) {
		return 1
	}

	a := slackReadThreadArgs{Channel: channel, TS: ts, Limit: limit}
	var out []slackMessageView
	if slackReportOnErr(slackCallExtension(ctx, host, "read_thread", a, &out)) {
		return 1
	}
	slackPrintJSON(out)
	return 0
}

func slackCLIReadChannel(ctx context.Context, args []string, host agenthost.Client) int {
	channel := slackFlagVal(args, "--channel")
	if channel == "" {
		slackReportErr("--channel is required")
		return 1
	}
	ts := slackFlagVal(args, "--ts")
	numPrior, err := slackIntFlag(args, "--num-prior")
	if slackReportOnErr(err) {
		return 1
	}
	numFollowing, err := slackIntFlag(args, "--num-following")
	if slackReportOnErr(err) {
		return 1
	}
	limit, err := slackIntFlag(args, "--limit")
	if slackReportOnErr(err) {
		return 1
	}
	if limit > 0 && (ts != "" || numPrior > 0 || numFollowing > 0) {
		slackReportErr("--limit and the --ts/--num-prior/--num-following anchored form are mutually exclusive")
		return 1
	}
	if (numPrior > 0 || numFollowing > 0) && ts == "" {
		slackReportErr("--num-prior/--num-following require --ts")
		return 1
	}
	if ts != "" && numPrior == 0 && numFollowing == 0 {
		slackReportErr("--ts requires --num-prior and/or --num-following (an anchor alone reads nothing)")
		return 1
	}

	a := slackReadChannelArgs{Channel: channel, TS: ts, NumPrior: numPrior, NumFollowing: numFollowing, Limit: limit}
	var out []slackMessageView
	if slackReportOnErr(slackCallExtension(ctx, host, "read_channel", a, &out)) {
		return 1
	}
	slackPrintJSON(out)
	return 0
}

// --- download ---

func slackCLIDownload(ctx context.Context, args []string, host agenthost.Client) int {
	fileID := slackFlagVal(args, "--id")
	if fileID == "" {
		slackReportErr("--id is required")
		return 1
	}

	a := slackDownloadArgs{FileID: fileID}
	var out slackDownloadResult
	if slackReportOnErr(slackCallExtension(ctx, host, "download", a, &out)) {
		return 1
	}

	data, err := base64.StdEncoding.DecodeString(out.Base64)
	if slackReportOnErr(err) {
		return 1
	}
	dest := slackFlagVal(args, "--out")
	if dest == "" {
		dest = out.Name
		if dest == "" {
			dest = fileID
		}
	}
	if err := os.WriteFile(dest, data, 0o644); slackReportOnErr(err) {
		return 1
	}
	slackPrintJSON(map[string]string{"path": dest})
	return 0
}
