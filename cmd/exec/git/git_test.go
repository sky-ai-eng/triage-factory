package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
)

// signHost answers SignCommit with res or err, recording the payload. It
// embeds agenthost.Client (nil) so it satisfies the interface; only SignCommit
// and Close are called.
type signHost struct {
	agenthost.Client
	res     agenthost.SignCommitResult
	err     error
	payload []byte
}

func (h *signHost) SignCommit(_ context.Context, payload []byte) (agenthost.SignCommitResult, error) {
	h.payload = payload
	return h.res, h.err
}

func (h *signHost) Close() error { return nil }

func writeBuffer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".git_signing_buffer_tmpABC")
	if err := os.WriteFile(path, []byte("tree x\n\nsubject\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func gitArgs(buffer string) []string {
	return []string{"sign", "-Y", "sign", "-n", "git", "-f", "/tmp/.git_signing_key_tmpXYZ", "-U", buffer}
}

func TestHandle_WritesTheSignature(t *testing.T) {
	buffer := writeBuffer(t)
	host := &signHost{res: agenthost.SignCommitResult{Signature: "-----BEGIN SSH SIGNATURE-----\nabc\n-----END SSH SIGNATURE-----\n"}}
	var stderr bytes.Buffer
	if code := Handle(context.Background(), func() agenthost.Client { return host }, gitArgs(buffer), &stderr); code != 0 {
		t.Fatalf("Handle = %d, stderr %q", code, stderr.String())
	}
	if string(host.payload) != "tree x\n\nsubject\n" {
		t.Errorf("SignCommit was handed %q, not the buffer", host.payload)
	}
	sig, err := os.ReadFile(buffer + ".sig")
	if err != nil || string(sig) != host.res.Signature {
		t.Errorf("%s.sig = %q, %v", buffer, sig, err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a signature printed %q", stderr.String())
	}
}

func TestHandle_PrintsTheRefusal(t *testing.T) {
	buffer := writeBuffer(t)
	line := "triagefactory: this run no longer holds its engagement, so commit signing refused. Nothing was committed."
	host := &signHost{res: agenthost.SignCommitResult{Refusal: line}}
	var stderr bytes.Buffer
	if code := Handle(context.Background(), func() agenthost.Client { return host }, gitArgs(buffer), &stderr); code != 1 {
		t.Fatalf("Handle = %d, want 1", code)
	}
	if stderr.String() != line+"\n" {
		t.Errorf("stderr = %q, want the refusal line", stderr.String())
	}
	if _, err := os.Stat(buffer + ".sig"); !os.IsNotExist(err) {
		t.Errorf("a refusal left a .sig file: %v", err)
	}
}

func TestHandle_AFailedCallIsUnanswered(t *testing.T) {
	buffer := writeBuffer(t)
	host := &signHost{err: errors.New("agenthost: dial /run/tf.sock: connection refused")}
	var stderr bytes.Buffer
	if code := Handle(context.Background(), func() agenthost.Client { return host }, gitArgs(buffer), &stderr); code != 1 {
		t.Fatalf("Handle = %d, want 1", code)
	}
	if stderr.String() != commitsigner.UnansweredLine()+"\n" {
		t.Errorf("stderr = %q, want the unanswered line", stderr.String())
	}
}

// Anything but git's signing call is refused before a host is built: the
// program answers git, and git makes no other call of it.
func TestHandle_RefusesWhatGitDoesNotSend(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"verify"},
		{"sign"},
		{"sign", "-Y", "verify", "-n", "git", "/tmp/buffer"},
		{"sign", "-Y", "sign", "-n", "file", "/tmp/buffer"},
		{"sign", "-Y", "sign", "-n", "git"},
		{"sign", "-Y", "sign", "-n", "git", "-O", "hashalg=sha256", "/tmp/buffer"},
		{"sign", "-Y", "sign", "-n", "git", "-f", "/tmp/buffer"},
		{"sign", "-Y", "sign", "-n", "git", ""},
	} {
		var stderr bytes.Buffer
		built := false
		code := Handle(context.Background(), func() agenthost.Client { built = true; return &signHost{} }, args, &stderr)
		if code != 1 || built || !strings.HasPrefix(stderr.String(), "triagefactory: ") {
			t.Errorf("Handle(%q) = %d (host built: %v), stderr %q", args, code, built, stderr.String())
		}
	}
}

func TestSignBuffer(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-Y", "sign", "-n", "git", "-f", "/k", "-U", "/b"}, "/b"},
		{[]string{"-Y", "sign", "-n", "git", "-U", "-f", "/k", "/b"}, "/b"},
		{[]string{"-Y", "sign", "-n", "git", "/b"}, "/b"},
	} {
		if got, ok := signBuffer(tc.args); !ok || got != tc.want {
			t.Errorf("signBuffer(%q) = %q, %v; want %q", tc.args, got, ok, tc.want)
		}
	}
}
