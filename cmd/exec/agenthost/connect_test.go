package agenthost

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// serveSocket binds a live agenthost daemon at path and returns it.
func serveSocket(t *testing.T, stores db.Stores, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-connect"}, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// deadSocket leaves a real socket file at path with nothing listening on
// it — the present-but-dead case. UnlinkOnClose is disabled so closing the
// listener keeps the inode; a Dial against it fails at connect time.
func deadSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
}

// TestDialSandbox pins the jailed constructor's three outcomes. The
// load-bearing one is the missing socket: it must fail closed rather than
// resolve identity locally, because a jail's TF_MODE is unset — so a local
// fallback would default to local mode, mint a fresh empty SQLite file inside
// the sandbox, and then report that the run did not exist. The run did exist;
// the diagnosis blamed the one component that was innocent.
func TestDialSandbox(t *testing.T) {
	t.Run("missing socket → fails closed", func(t *testing.T) {
		c, err := dialSandbox(context.Background(), tempSocket(t))
		if c != nil {
			t.Errorf("got client %T, want nil", c)
		}
		if !errors.Is(err, ErrSandboxSocketMissing) {
			t.Fatalf("got %v, want ErrSandboxSocketMissing", err)
		}
	})

	t.Run("live socket → IPC", func(t *testing.T) {
		stores, conn := newTestDB(t)
		seedConversation(t, stores, conn, "run-connect", runmode.LocalDefaultUserID, "manual")
		sock := tempSocket(t)
		serveSocket(t, stores, sock)
		c, err := dialSandbox(context.Background(), sock)
		if err != nil {
			t.Fatalf("dialSandbox: %v", err)
		}
		defer c.Close()
		if _, ok := c.(*IPCClient); !ok {
			t.Errorf("got %T, want *IPCClient", c)
		}
	})

	t.Run("dead socket → ErrDaemonUnreachable", func(t *testing.T) {
		sock := tempSocket(t)
		deadSocket(t, sock)
		c, err := dialSandbox(context.Background(), sock)
		if c != nil {
			t.Errorf("got client %T, want nil", c)
		}
		if !errors.Is(err, ErrDaemonUnreachable) {
			t.Fatalf("got %v, want ErrDaemonUnreachable", err)
		}
	})
}

// TestNewLocalFromEnv pins the host-side constructor: run identity resolves
// from TRIAGE_FACTORY_CONVERSATION_ID against the supplied stores, and the
// client serves calls in-process.
func TestNewLocalFromEnv(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-local-env", runmode.LocalDefaultUserID, "manual")
	t.Setenv("TRIAGE_FACTORY_CONVERSATION_ID", "run-local-env")

	c, err := NewLocalFromEnv(context.Background(), stores)
	if err != nil {
		t.Fatalf("NewLocalFromEnv: %v", err)
	}
	defer c.Close()
	if _, ok := c.(*LocalClient); !ok {
		t.Fatalf("got %T, want *LocalClient", c)
	}
	got, err := c.LookupRun(context.Background())
	if err != nil {
		t.Fatalf("LookupRun: %v", err)
	}
	if got.RunID != "run-local-env" {
		t.Errorf("RunID: got %q, want run-local-env", got.RunID)
	}
}

// TestErrSandboxSocketMissing_Wording pins what the message must and must
// not say. The point of the error is that the agent reading it stops
// investigating its own command and the operator knows where to look, so
// the socket path and the "launch wiring bug" attribution are contract —
// as is the absence of the old spawner-injection blame, which sent the
// first live dogfood run on a four-turn hunt through env vars.
func TestErrSandboxSocketMissing_Wording(t *testing.T) {
	msg := ErrSandboxSocketMissing.Error()
	for _, want := range []string{DefaultSocketPath, "launch wiring bug"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error text %q does not mention %q", msg, want)
		}
	}
	for _, unwanted := range []string{"spawner", "does not exist"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("error text %q mentions %q; the run is not the problem", msg, unwanted)
		}
	}
}
