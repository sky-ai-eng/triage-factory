package slack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
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

func TestRunSlackCLI_NoArgs_UsageError(t *testing.T) {
	host := &fakeExtensionHost{}
	code := runSlackCLI(context.Background(), nil, host)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
