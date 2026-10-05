//go:build linux

package agenthost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestStartWithServer_KeepsTheJailSocketPath is the drift guard for the
// socket-root parameterization. The multi-mode path is validated broker-side
// against a path the broker re-derives independently, so a refactor that
// relocated it — even by a directory — would be rejected at every launch. The
// relocation is additive: only StartLocal's socket moved.
func TestStartWithServer_KeepsTheJailSocketPath(t *testing.T) {
	for _, id := range []string{"conv-1", "00000000-0000-0000-0000-0000000000aa"} {
		if got, want := SocketPathFor(id), filepath.Join("/run/tf", id+".sock"); got != want {
			t.Errorf("SocketPathFor(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestStartLocal_ServesOnTheGivenPathOwnerOnly pins the local half: the
// daemon listens exactly where the caller said (the state root, which an
// unprivileged process can write, unlike /run/tf), and the socket is 0600 —
// the agent in the local namespace runs as THIS uid, so owner-only is both
// the tightest grant and the only one that would work.
func TestStartLocal_ServesOnTheGivenPathOwnerOnly(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "agenthost", "conv-local.sock")

	hd, err := StartLocal(db.Stores{}, ConversationInfo{ConversationID: "conv-local", OrgID: "org-1"}, sockPath)
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}
	defer func() { _ = hd.Close() }()

	if hd.SocketPath() != sockPath {
		t.Errorf("SocketPath() = %q, want %q", hd.SocketPath(), sockPath)
	}
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", perm)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket (mode %v)", sockPath, info.Mode())
	}

	// The parent dir is owner-only for the same reason the jail's root is:
	// nothing else on a shared machine may reach a run's socket.
	dirInfo, err := os.Stat(filepath.Dir(sockPath))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket dir mode = %04o, want 0700", perm)
	}

	// And the client reaches it: this is the whole point of relocating the
	// socket rather than teaching the jailed CLI a second transport.
	client, err := dialSandbox(context.Background(), sockPath)
	if err != nil {
		t.Fatalf("dialSandbox against the local socket: %v", err)
	}
	defer func() { _ = client.Close() }()
	info2, err := client.LookupConversation(context.Background())
	if err != nil {
		t.Fatalf("LookupConversation: %v", err)
	}
	if info2.ConversationID != "conv-local" {
		t.Errorf("LookupConversation returned %q, want conv-local", info2.ConversationID)
	}
}

// TestStartLocal_TightensAnExistingSocketRoot pins the half MkdirAll cannot
// do: its mode applies only to a directory it actually creates, so from the
// second run of an install onward the socket root's mode is whatever the
// first run's umask left. An operator with a permissive umask would otherwise
// end up with a root other accounts on the machine can list.
func TestStartLocal_TightensAnExistingSocketRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agenthost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // defeat the umask
		t.Fatal(err)
	}

	hd, err := StartLocal(db.Stores{}, ConversationInfo{ConversationID: "conv"}, filepath.Join(dir, "conv.sock"))
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}
	defer func() { _ = hd.Close() }()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("pre-existing socket root left at %04o, want 0700", perm)
	}
}

// TestStartLocal_RemovesTheSocketOnClose pins that a run leaves nothing
// behind: a stale socket file in the state root would make the next
// engagement's dial land on a dead inode instead of failing to find one.
func TestStartLocal_RemovesTheSocketOnClose(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "conv.sock")
	hd, err := StartLocal(db.Stores{}, ConversationInfo{ConversationID: "conv"}, sockPath)
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}
	if err := hd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket still present after Close (err=%v)", err)
	}
}

// TestStartLocal_RejectsAnIncompleteRequest pins that the daemon refuses to
// come up without the two things it cannot do its job without, rather than
// serving a socket nobody can attribute.
func TestStartLocal_RejectsAnIncompleteRequest(t *testing.T) {
	if _, err := StartLocal(db.Stores{}, ConversationInfo{}, filepath.Join(t.TempDir(), "x.sock")); err == nil {
		t.Error("StartLocal accepted a ConversationInfo with no conversation id")
	}
	if _, err := StartLocal(db.Stores{}, ConversationInfo{ConversationID: "c"}, ""); err == nil {
		t.Error("StartLocal accepted an empty socket path")
	} else if !strings.Contains(err.Error(), "socket path") {
		t.Errorf("error %q does not name the missing socket path", err)
	}
}

// TestStartLocal_AnswersForTheAgentsOwnNamespace pins the two answers a local
// run's daemon gives differently from a jail's. The local namespace binds the
// run root back at its host path, so WorkspaceRoots gives the agent that path
// and not /work. The agent also runs as this uid, so a `workspace add`
// checkout comes back still owned by this process. A jail's daemon would hand
// it to the sandbox uid, which an unprivileged process cannot do.
func TestStartLocal_AnswersForTheAgentsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "conv-local-ws", runmode.LocalDefaultUserID, "manual")
	runRoot := filepath.Join(t.TempDir(), "triagefactory-runs", "task-local")
	if _, err := stores.Conversations.SetWorktreePathSystem(ctx, runmode.LocalDefaultOrgID, "conv-local-ws", runRoot); err != nil {
		t.Fatalf("SetWorktreePathSystem: %v", err)
	}
	seedWorkspaceRepo(t, stores, "sky", "core", "https://github.com/sky/core.git")
	checkout := filepath.Join(runRoot, "sky", "core", "main")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubWorkspaceCreates(t, &createRecorder{path: checkout})

	hd, err := StartLocal(stores, workspaceInfo("conv-local-ws"), filepath.Join(t.TempDir(), "agenthost", "conv-local-ws.sock"))
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}
	defer func() { _ = hd.Close() }()
	client, err := dialSandbox(ctx, hd.SocketPath())
	if err != nil {
		t.Fatalf("dialSandbox: %v", err)
	}
	defer func() { _ = client.Close() }()

	host, agent, err := client.WorkspaceRoots(ctx)
	if err != nil {
		t.Fatalf("WorkspaceRoots: %v", err)
	}
	if host != runRoot || agent != runRoot {
		t.Errorf("roots = (%q, %q), want the host run root for both", host, agent)
	}

	path, err := client.CreateWorkspaceCheckout(ctx, "sky", "core", "main", 0)
	if err != nil {
		t.Fatalf("workspace add over the local socket: %v", err)
	}
	if path != checkout {
		t.Errorf("path = %q, want %q", path, checkout)
	}
	for _, p := range []string{filepath.Join(runRoot, "sky"), checkout, filepath.Join(checkout, "README.md")} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		if uid := fi.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Getuid() {
			t.Errorf("%s is owned by uid %d, want this process's uid %d", p, uid, os.Getuid())
		}
	}
}
