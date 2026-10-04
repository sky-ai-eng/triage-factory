package upstream

import (
	"context"
	"net/url"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestFailFast_ScopedPerHost pins the scope: outside one nothing is ever
// unreachable and marking does nothing; inside one a marked host is
// unreachable for every request under the scope, other hosts are not, and a
// new scope starts empty.
func TestFailFast_ScopedPerHost(t *testing.T) {
	plain := context.Background()
	MarkUnreachable(plain, "ghes.corp.example")
	if Unreachable(plain, "ghes.corp.example") {
		t.Error("a host is unreachable outside any fail-fast scope")
	}

	scope := WithFailFast(plain)
	if Unreachable(scope, "ghes.corp.example") {
		t.Error("a host is unreachable in a scope that has seen no failure")
	}
	child, cancel := context.WithCancel(scope)
	defer cancel()
	MarkUnreachable(child, "ghes.corp.example")
	if !Unreachable(scope, "ghes.corp.example") {
		t.Error("a host marked through a derived context is not unreachable in its scope")
	}
	if Unreachable(scope, "api.atlassian.com") {
		t.Error("marking one host made another unreachable")
	}
	if Unreachable(WithFailFast(scope), "ghes.corp.example") {
		t.Error("a new scope inherited its parent's unreachable hosts")
	}
}

// TestFailFast_SilentAfterTwoTimeoutsUntilTheHostAnswers pins which failures
// stop a scope sending to a host: a second timeout with no answer from the
// host since the first does, a single timeout does not, a refused connection
// never does, and an answer from the host lifts it while leaving the host
// unreachable. A timeout of a request the host has answered another request
// since does not count, because the host was serving while it hung.
//
// One timeout is not enough because a poll cycle sends its requests one after
// another: nothing can answer while a slow request is out, so a single heavy
// read that always exceeds the client timeout would silence the host for the
// rest of every cycle.
func TestFailFast_SilentAfterTwoTimeoutsUntilTheHostAnswers(t *testing.T) {
	timeout := &url.Error{Op: "Get", URL: "https://ghes.corp.example/x", Err: context.DeadlineExceeded}
	refused := &url.Error{Op: "Get", URL: "https://ghes.corp.example/x", Err: syscall.ECONNREFUSED}

	plain := context.Background()
	MarkTransportFailure(plain, "ghes.corp.example", time.Now(), timeout)
	MarkTransportFailure(plain, "ghes.corp.example", time.Now(), timeout)
	if Silent(plain, "ghes.corp.example") {
		t.Error("a host is silent outside any fail-fast scope")
	}

	scope := WithFailFast(plain)
	MarkTransportFailure(scope, "ghes.corp.example", time.Now(), refused)
	if !Unreachable(scope, "ghes.corp.example") {
		t.Error("a refused host is not unreachable")
	}
	MarkTransportFailure(scope, "ghes.corp.example", time.Now(), refused)
	if Silent(scope, "ghes.corp.example") {
		t.Error("refused connections made the host silent")
	}

	MarkTransportFailure(scope, "ghes.corp.example", time.Now(), timeout)
	if Silent(scope, "ghes.corp.example") {
		t.Fatal("one timeout made the host silent")
	}
	MarkTransportFailure(scope, "ghes.corp.example", time.Now(), timeout)
	if !Silent(scope, "ghes.corp.example") {
		t.Fatal("a second timeout with no answer between did not make the host silent")
	}
	if Silent(scope, "api.atlassian.com") {
		t.Error("timeouts against one host made another silent")
	}

	MarkAnswered(scope, "ghes.corp.example")
	if Silent(scope, "ghes.corp.example") {
		t.Error("a host that answered is still silent")
	}
	if !Unreachable(scope, "ghes.corp.example") {
		t.Error("an answer cleared the host's unreachable mark")
	}
	MarkTransportFailure(scope, "ghes.corp.example", time.Now(), timeout)
	if Silent(scope, "ghes.corp.example") {
		t.Error("a timeout before the answer still counted toward silence after it")
	}

	MarkAnswered(scope, "ghes.corp.example")
	sent := time.Now()
	MarkAnswered(scope, "ghes.corp.example")
	MarkTransportFailure(scope, "ghes.corp.example", sent, timeout)
	MarkTransportFailure(scope, "ghes.corp.example", time.Now(), timeout)
	if Silent(scope, "ghes.corp.example") {
		t.Error("a timeout counted toward silence though the host answered another request while it was out")
	}
}

// TestFailFast_ConcurrentUse is for the race detector: a poll cycle marks and
// reads one scope from its whole fan-out.
func TestFailFast_ConcurrentUse(t *testing.T) {
	scope := WithFailFast(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); MarkUnreachable(scope, "h") }()
		go func() { defer wg.Done(); _ = Unreachable(scope, "h") }()
	}
	wg.Wait()
	if !Unreachable(scope, "h") {
		t.Error("host not unreachable after concurrent marks")
	}
}
