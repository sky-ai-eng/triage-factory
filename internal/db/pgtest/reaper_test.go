package pgtest

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// fakeReaper accepts one connection, reads the filter line onto lines, and
// hands the server end of the connection to reply, which decides how the
// reaper answers.
func fakeReaper(t *testing.T, reply func(conn net.Conn)) (endpoint string, lines <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return
		}
		got <- line
		reply(conn)
	}()
	return ln.Addr().String(), got
}

// TestRegisterWithReaper_HoldsAcknowledgedConnection pins the success
// half of the protocol: the session's labels go out as the filter line,
// and an acknowledged connection comes back open, because closing it is
// what lets the reaper prune.
func TestRegisterWithReaper_HoldsAcknowledgedConnection(t *testing.T) {
	serverClosed := make(chan error, 1)
	endpoint, lines := fakeReaper(t, func(conn net.Conn) {
		if _, err := conn.Write([]byte(reaperAck)); err != nil {
			serverClosed <- err
			return
		}
		// A read on the server side ends only when the client closes.
		_, err := conn.Read(make([]byte, 1))
		serverClosed <- err
	})

	conn, err := registerWithReaper(t.Context(), endpoint)
	if err != nil {
		t.Fatalf("registerWithReaper: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	line := <-lines
	if want := reaperFilter(testcontainers.GenericLabels()); line != want {
		t.Errorf("filter line = %q, want %q", line, want)
	}
	if want := "label=org.testcontainers.sessionId=" + testcontainers.SessionID(); !strings.Contains(line, want) {
		t.Errorf("filter line %q does not name this session (%q)", line, want)
	}

	select {
	case err := <-serverClosed:
		t.Fatalf("connection closed after the acknowledgement (%v); the reaper would count no client", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRegisterWithReaper_RefusedWithoutAck pins the failure half: a
// reaper in its final prune accepts the connection and closes it without
// answering, and that must surface as an error rather than as a
// registration, since such a reaper protects nothing.
func TestRegisterWithReaper_RefusedWithoutAck(t *testing.T) {
	endpoint, _ := fakeReaper(t, func(conn net.Conn) { _ = conn.Close() })

	conn, err := registerWithReaper(t.Context(), endpoint)
	if err == nil {
		_ = conn.Close()
		t.Fatal("registerWithReaper succeeded against a reaper that closed without acknowledging")
	}
}

// TestRegisterWithReaper_RejectsWrongAnswer covers a listener that
// answers with something other than the acknowledgement.
func TestRegisterWithReaper_RejectsWrongAnswer(t *testing.T) {
	endpoint, _ := fakeReaper(t, func(conn net.Conn) { _, _ = conn.Write([]byte("NAK\n")) })

	conn, err := registerWithReaper(t.Context(), endpoint)
	if err == nil {
		_ = conn.Close()
		t.Fatal("registerWithReaper accepted an answer other than the acknowledgement")
	}
}
