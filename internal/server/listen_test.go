package server

import (
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// TestListenAndServeContext_BindErrorDoesNotLeak guards the shutdown watcher
// against leaking when the server stops on its own — here a bind failure —
// before ctx is cancelled. ListenAndServe passes context.Background(), whose
// Done channel never closes, so a watcher that only waited on ctx.Done()
// would park forever.
func TestListenAndServeContext_BindErrorDoesNotLeak(t *testing.T) {
	// Occupy a port so the server's bind fails deterministically.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()

	s := &Server{mux: http.NewServeMux()}

	before := runtime.NumGoroutine()
	if err := s.ListenAndServe(ln.Addr().String()); err == nil {
		t.Fatal("expected a bind error from an in-use address, got nil")
	}

	// The watcher is joined before ListenAndServeContext returns, so the
	// count settles back immediately. The pre-fix version left a goroutine
	// parked on a Done channel that never closed, so this loop would time out.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak after bind failure: have %d, want <= %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
