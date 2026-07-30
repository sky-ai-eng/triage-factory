package agenthost

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// serveSocket binds a live agenthost daemon at path and returns it.
func serveSocket(t *testing.T, stores db.Stores, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-autodetect"}, nil)
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

// failOnUseConversations is the "stores must not be consulted" probe. The
// embedded nil interface satisfies db.ConversationStore, so any method the
// resolver might reach for either fails the test explicitly (the two below,
// which is what the local branch actually calls) or panics on the nil embed.
type failOnUseConversations struct {
	db.ConversationStore
	t *testing.T
}

func (f failOnUseConversations) LookupOrgForRunSystem(context.Context, string) (string, error) {
	f.t.Error("AutoDetect consulted stores inside the sandbox; the missing socket must fail closed before any DB access")
	return "", nil
}

func (f failOnUseConversations) GetSystem(context.Context, string, string) (*domain.Conversation, error) {
	f.t.Error("AutoDetect consulted stores inside the sandbox; the missing socket must fail closed before any DB access")
	return nil, nil
}

// TestAutoDetect_Matrix pins the full (sandbox marker × socket) matrix.
//
// The load-bearing cell is (marker set, no socket): before TFAC-706 it fell
// through to the local branch, which opens a database per TF_MODE — unset in
// a jail, so it defaults to local and mints a fresh empty SQLite file inside
// the sandbox — and then reported that the run did not exist. The run did
// exist; the diagnosis blamed the one component that was innocent.
func TestAutoDetect_Matrix(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "run-matrix", runmode.LocalDefaultUserID, "manual")
	t.Setenv("TRIAGE_FACTORY_CONVERSATION_ID", "run-matrix")

	t.Run("no marker, no socket → LocalClient", func(t *testing.T) {
		c, err := autoDetect(context.Background(), stores, tempSocket(t), false)
		if err != nil {
			t.Fatalf("autoDetect: %v", err)
		}
		defer c.Close()
		if _, ok := c.(*LocalClient); !ok {
			t.Errorf("got %T, want *LocalClient", c)
		}
	})

	t.Run("no marker, live socket → IPC", func(t *testing.T) {
		sock := tempSocket(t)
		serveSocket(t, stores, sock)
		c, err := autoDetect(context.Background(), stores, sock, false)
		if err != nil {
			t.Fatalf("autoDetect: %v", err)
		}
		defer c.Close()
		if _, ok := c.(*IPCClient); !ok {
			t.Errorf("got %T, want *IPCClient", c)
		}
	})

	t.Run("marker, live socket → IPC", func(t *testing.T) {
		sock := tempSocket(t)
		serveSocket(t, stores, sock)
		c, err := autoDetect(context.Background(), stores, sock, true)
		if err != nil {
			t.Fatalf("autoDetect: %v", err)
		}
		defer c.Close()
		if _, ok := c.(*IPCClient); !ok {
			t.Errorf("got %T, want *IPCClient", c)
		}
	})

	t.Run("marker, no socket → fails closed without touching stores", func(t *testing.T) {
		probe := db.Stores{Conversations: failOnUseConversations{t: t}}
		c, err := autoDetect(context.Background(), probe, tempSocket(t), true)
		if c != nil {
			t.Errorf("got client %T, want nil", c)
		}
		if !errors.Is(err, ErrSandboxSocketMissing) {
			t.Fatalf("got %v, want ErrSandboxSocketMissing", err)
		}
	})

	t.Run("marker, dead socket → ErrDaemonUnreachable", func(t *testing.T) {
		sock := tempSocket(t)
		deadSocket(t, sock)
		c, err := autoDetect(context.Background(), stores, sock, true)
		if c != nil {
			t.Errorf("got client %T, want nil", c)
		}
		if !errors.Is(err, ErrDaemonUnreachable) {
			t.Fatalf("got %v, want ErrDaemonUnreachable", err)
		}
	})
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

// TestInSandbox_ExactMatch pins the marker read: only the assembler's exact
// value counts. A truthy-looking value from somewhere else is not the
// assembler saying "you are in a jail", and reading it as one would break
// local mode for anyone with a stray export.
func TestInSandbox_ExactMatch(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{agentproc.SandboxMarkerEnvValue, true},
		{"", false},
		{"0", false},
		{"true", false},
		{"1 ", false},
	} {
		t.Setenv(agentproc.SandboxMarkerEnvVar, tc.value)
		if got := inSandbox(); got != tc.want {
			t.Errorf("%s=%q: inSandbox() = %v, want %v", agentproc.SandboxMarkerEnvVar, tc.value, got, tc.want)
		}
	}
}
