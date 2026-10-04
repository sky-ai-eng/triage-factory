package upstream

import (
	"context"
	"sync"
	"testing"
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
