package slack

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
)

// fakeExtensionHost is a minimal agenthost.Client fake for the CLI's
// arg-parse tests: it embeds the (nil) interface so any method besides
// CallExtension panics if a verb ever reached for it — every `exec slack`
// verb makes exactly one CallExtension call and nothing else, so that's the
// contract this pins.
type fakeExtensionHost struct {
	agenthost.Client
	gotNamespace, gotMethod string
	gotArgs                 json.RawMessage
	result                  json.RawMessage
	err                     error
	calls                   int
}

func (f *fakeExtensionHost) CallExtension(_ context.Context, namespace, method string, args json.RawMessage) (json.RawMessage, error) {
	f.calls++
	f.gotNamespace, f.gotMethod, f.gotArgs = namespace, method, args
	return f.result, f.err
}

func decodeArgs[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode args %s: %v", raw, err)
	}
	return out
}

// --- send ---

func TestSlackCLISend_ValidBody_GoldenArgs(t *testing.T) {
	host := &fakeExtensionHost{result: json.RawMessage(`{"channel":"C1","ts":"1.1"}`)}
	code := slackCLISend(context.Background(), []string{"--channel", "C1", "--thread-ts", "1.0", "--body", "hello"}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if host.gotNamespace != "slack" || host.gotMethod != "send" {
		t.Fatalf("namespace/method = %q/%q, want slack/send", host.gotNamespace, host.gotMethod)
	}
	got := decodeArgs[slackSendArgs](t, host.gotArgs)
	want := slackSendArgs{Channel: "C1", ThreadTS: "1.0", Body: "hello"}
	if got != want {
		t.Errorf("args = %+v, want %+v", got, want)
	}
}

func TestSlackCLISend_BodyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte("from a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := &fakeExtensionHost{result: json.RawMessage(`{}`)}
	code := slackCLISend(context.Background(), []string{"--channel", "C1", "--body-file", path}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := decodeArgs[slackSendArgs](t, host.gotArgs)
	if got.Body != "from a file" {
		t.Errorf("Body = %q, want %q", got.Body, "from a file")
	}
}

func TestSlackCLISend_AttachFileOnly_NoBodyRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot.png")
	if err := os.WriteFile(path, []byte("binary-ish content"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := &fakeExtensionHost{result: json.RawMessage(`{}`)}
	code := slackCLISend(context.Background(), []string{"--channel", "C1", "--attach-file", path}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := decodeArgs[slackSendArgs](t, host.gotArgs)
	if got.Body != "" {
		t.Errorf("Body = %q, want empty (file-only message)", got.Body)
	}
	if got.AttachName != "screenshot.png" {
		t.Errorf("AttachName = %q, want screenshot.png", got.AttachName)
	}
	if got.AttachBase64 == "" {
		t.Error("AttachBase64 must not be empty")
	}
}

func TestSlackCLISend_AttachFileTooLarge_UsageError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(slackExecMaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	host := &fakeExtensionHost{}
	code := slackCLISend(context.Background(), []string{"--channel", "C1", "--attach-file", path}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host (or base64-encode the file) when --attach-file exceeds the size cap")
	}
}

func TestSlackCLISend_MissingChannel_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLISend(context.Background(), []string{"--body", "hi"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when required flags are missing")
	}
}

func TestSlackCLISend_NoBodyNoAttach_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLISend(context.Background(), []string{"--channel", "C1"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when there's nothing to send")
	}
}

func TestSlackCLISend_BodyAndBodyFileConflict_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLISend(context.Background(), []string{"--channel", "C1", "--body", "a", "--body-file", "b"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host on a --body/--body-file conflict")
	}
}

// --- edit ---

func TestSlackCLIEdit_Valid_GoldenArgs(t *testing.T) {
	host := &fakeExtensionHost{result: json.RawMessage(`{}`)}
	code := slackCLIEdit(context.Background(), []string{"--channel", "C1", "--ts", "1.1", "--body", "edited"}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if host.gotMethod != "edit" {
		t.Errorf("method = %q, want edit", host.gotMethod)
	}
	got := decodeArgs[slackEditArgs](t, host.gotArgs)
	want := slackEditArgs{Channel: "C1", TS: "1.1", Body: "edited"}
	if got != want {
		t.Errorf("args = %+v, want %+v", got, want)
	}
}

func TestSlackCLIEdit_MissingTS_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIEdit(context.Background(), []string{"--channel", "C1", "--body", "x"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when --ts is missing")
	}
}

func TestSlackCLIEdit_MissingBody_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIEdit(context.Background(), []string{"--channel", "C1", "--ts", "1.1"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when the body is missing")
	}
}

// --- react ---

func TestSlackCLIReact_Valid_GoldenArgs(t *testing.T) {
	host := &fakeExtensionHost{result: json.RawMessage(`{}`)}
	code := slackCLIReact(context.Background(), []string{"--channel", "C1", "--ts", "1.1", "--emoji", "thumbsup"}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if host.gotMethod != "react" {
		t.Errorf("method = %q, want react", host.gotMethod)
	}
	got := decodeArgs[slackReactArgs](t, host.gotArgs)
	want := slackReactArgs{Channel: "C1", TS: "1.1", Emoji: "thumbsup"}
	if got != want {
		t.Errorf("args = %+v, want %+v", got, want)
	}
}

func TestSlackCLIReact_MissingEmoji_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIReact(context.Background(), []string{"--channel", "C1", "--ts", "1.1"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when --emoji is missing")
	}
}

// --- read thread / read channel ---

func TestSlackCLIReadThread_Valid_GoldenArgs(t *testing.T) {
	host := &fakeExtensionHost{result: json.RawMessage(`[]`)}
	code := slackCLIRead(context.Background(), []string{"thread", "--channel", "C1", "--ts", "1.0", "--limit", "10"}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if host.gotMethod != "read_thread" {
		t.Errorf("method = %q, want read_thread", host.gotMethod)
	}
	got := decodeArgs[slackReadThreadArgs](t, host.gotArgs)
	want := slackReadThreadArgs{Channel: "C1", TS: "1.0", Limit: 10}
	if got != want {
		t.Errorf("args = %+v, want %+v", got, want)
	}
}

func TestSlackCLIReadThread_MissingTS_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIRead(context.Background(), []string{"thread", "--channel", "C1"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when --ts is missing")
	}
}

func TestSlackCLIReadThread_BadLimit_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIRead(context.Background(), []string{"thread", "--channel", "C1", "--ts", "1.0", "--limit", "notanumber"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host with an unparsable --limit")
	}
}

func TestSlackCLIReadChannel_LimitOnly_GoldenArgs(t *testing.T) {
	host := &fakeExtensionHost{result: json.RawMessage(`[]`)}
	code := slackCLIRead(context.Background(), []string{"channel", "--channel", "C1", "--limit", "5"}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if host.gotMethod != "read_channel" {
		t.Errorf("method = %q, want read_channel", host.gotMethod)
	}
	got := decodeArgs[slackReadChannelArgs](t, host.gotArgs)
	want := slackReadChannelArgs{Channel: "C1", Limit: 5}
	if got != want {
		t.Errorf("args = %+v, want %+v", got, want)
	}
}

func TestSlackCLIReadChannel_Anchored_GoldenArgs(t *testing.T) {
	host := &fakeExtensionHost{result: json.RawMessage(`[]`)}
	code := slackCLIRead(context.Background(), []string{
		"channel", "--channel", "C1", "--ts", "1.0", "--num-prior", "3", "--num-following", "2",
	}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := decodeArgs[slackReadChannelArgs](t, host.gotArgs)
	want := slackReadChannelArgs{Channel: "C1", TS: "1.0", NumPrior: 3, NumFollowing: 2}
	if got != want {
		t.Errorf("args = %+v, want %+v", got, want)
	}
}

func TestSlackCLIReadChannel_LimitAndAnchor_MutuallyExclusive(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIRead(context.Background(), []string{
		"channel", "--channel", "C1", "--ts", "1.0", "--num-prior", "3", "--limit", "5",
	}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host on a --limit/anchored conflict")
	}
}

func TestSlackCLIReadChannel_NumPriorWithoutTS_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIRead(context.Background(), []string{"channel", "--channel", "C1", "--num-prior", "3"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when --num-prior lacks --ts")
	}
}

// TestSlackCLIReadChannel_TSAloneWithoutAnchor_UsageError pins that --ts
// with neither --num-prior nor --num-following is rejected rather than
// silently falling back to a plain latest-N read that ignores the anchor —
// the host's readChannel only takes the anchored branch when at least one
// of num_prior/num_following is set, so an unaccompanied --ts would
// otherwise vanish without a trace.
func TestSlackCLIReadChannel_TSAloneWithoutAnchor_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIRead(context.Background(), []string{"channel", "--channel", "C1", "--ts", "1.0"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when --ts has no --num-prior/--num-following to anchor")
	}
}

func TestSlackCLIRead_UnknownMode_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIRead(context.Background(), []string{"bogus", "--channel", "C1"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host for an unknown read mode")
	}
}

// --- download ---

func TestSlackCLIDownload_Valid_WritesFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.txt")
	host := &fakeExtensionHost{result: json.RawMessage(`{"name":"out.txt","base64":"aGVsbG8="}`)}
	code := slackCLIDownload(context.Background(), []string{"--id", "F123", "--out", dest}, host)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := decodeArgs[slackDownloadArgs](t, host.gotArgs)
	if got.FileID != "F123" {
		t.Errorf("FileID = %q, want F123", got.FileID)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("written content = %q, want %q", data, "hello")
	}
}

func TestSlackCLIDownload_MissingID_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := slackCLIDownload(context.Background(), nil, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host when --id is missing")
	}
}

// --- top-level dispatch ---

func TestRunSlackCLI_UnknownVerb_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := runSlackCLI(context.Background(), []string{"bogus"}, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if host.calls != 0 {
		t.Error("must not call the host for an unknown verb")
	}
}

// --- help routing ---

// runSlackCLICapture runs runSlackCLI with a NIL host — the dispatcher's
// help-route contract — capturing stdout. Any path that reached a verb body
// would nil-panic on the host, so a clean return IS the assertion that help
// routed before the verb.
func runSlackCLICapture(t *testing.T, args []string) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	code := runSlackCLI(context.Background(), args, nil)
	os.Stdout = saved
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out), code
}

// TestRunSlackCLI_FamilyHelp pins that `slack --help` — and bare `slack`,
// which used to be a usage error — prints the family usage under the invoked
// prefix and exits 0 with no host in scope. The exact invocation that used to
// fail with "unknown exec slack verb: --help".
func TestRunSlackCLI_FamilyHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"-h"}} {
		out, code := runSlackCLICapture(t, args)
		if code != 0 {
			t.Errorf("args %v: exit = %d, want 0", args, code)
		}
		if !strings.Contains(out, "Slack Commands:") || !strings.Contains(out, "slack send --channel") {
			t.Errorf("args %v: family help missing from output:\n%s", args, out)
		}
	}
}

// TestRunSlackCLI_VerbHelp pins per-verb help at every depth — `slack send
// --help`, `slack read thread --help` — served without a host and without the
// verb body's required-flag validation firing.
func TestRunSlackCLI_VerbHelp(t *testing.T) {
	for verb, wantLine := range map[string]string{
		"send":     "slack send --channel",
		"edit":     "slack edit --channel",
		"react":    "slack react --channel",
		"read":     "slack read thread --channel",
		"download": "slack download --id",
	} {
		out, code := runSlackCLICapture(t, []string{verb, "--help"})
		if code != 0 {
			t.Errorf("%s --help: exit = %d, want 0", verb, code)
		}
		if !strings.Contains(out, wantLine) {
			t.Errorf("%s --help output missing %q:\n%s", verb, wantLine, out)
		}
		if strings.Contains(out, "required") {
			t.Errorf("%s --help ran the verb body (required-flag error):\n%s", verb, out)
		}
	}

	// Depth below the verb routes identically — the scan covers the whole tail.
	out, code := runSlackCLICapture(t, []string{"read", "thread", "--help"})
	if code != 0 || !strings.Contains(out, "slack read thread --channel") {
		t.Errorf("read thread --help: exit=%d output:\n%s", code, out)
	}
}

// TestRunSlackCLI_ValueFlagPayloadIsNotHelp pins the guard the shared scan
// exists for: a value-taking flag whose payload is literally "--help" must
// not read as a help request — the verb body runs and reaches the host.
func TestRunSlackCLI_ValueFlagPayloadIsNotHelp(t *testing.T) {
	if execflags.HasHelpFlag([]string{"send", "--channel", "C1", "--body", "--help"}, cliValueFlags) {
		t.Error(`--body "--help" was read as a help request`)
	}
	host := &fakeExtensionHost{result: json.RawMessage(`{}`)}
	code := runSlackCLI(context.Background(), []string{"send", "--channel", "C1", "--body", "--help"}, host)
	if code != 0 || host.calls != 1 {
		t.Errorf(`send with --body "--help": exit=%d calls=%d, want a normal send`, code, host.calls)
	}
}

// TestRunSlackCLI_VerbHelpMirrorsFamilyHelp holds the per-verb usage lines
// and the family overview to one another: every slackVerbHelp line must
// appear in cliHelpText, so a flag rename cannot land in one and strand the
// other.
func TestRunSlackCLI_VerbHelpMirrorsFamilyHelp(t *testing.T) {
	for verb, lines := range slackVerbHelp {
		for _, line := range lines {
			if !strings.Contains(cliHelpText, line) {
				t.Errorf("verb %q usage line is not mirrored in cliHelpText: %s", verb, line)
			}
		}
	}
}

// TestRunSlackCLI_UnknownVerbWithHelpNamesTheValidSet pins the loser path: a
// mistyped verb with --help reports the valid set (the verb itself is wrong)
// instead of printing usage for a verb that does not exist.
func TestRunSlackCLI_UnknownVerbWithHelpNamesTheValidSet(t *testing.T) {
	_, code := runSlackCLICapture(t, []string{"bogus", "--help"})
	if code != 1 {
		t.Errorf("unknown verb with --help: exit = %d, want 1", code)
	}
}
