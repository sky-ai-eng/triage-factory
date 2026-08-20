package agenthost

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestUpstreamForMethod(t *testing.T) {
	cases := map[string]upstreamKind{
		methodJiraGetIssue:            upstreamJira,
		methodJiraCreateIssue:         upstreamJira,
		methodJiraSearchIssues:        upstreamJira,
		methodGithubGetPR:             upstreamGitHub,
		methodGithubSubmitReview:      upstreamGitHub,
		methodGithubDownloadArtifact:  upstreamGitHub,
		methodGithubAPIGet:            upstreamGitHub,
		methodLookupConversation:      upstreamNone,
		methodGetTask:                 upstreamNone,
		methodCreateWorkspaceCheckout: upstreamNone,
		methodUpsertArtifact:          upstreamNone,
	}
	for method, want := range cases {
		if got := upstreamForMethod(method); got != want {
			t.Errorf("upstreamForMethod(%q) = %d, want %d", method, got, want)
		}
	}
}

func TestUpstreamThrottle_NilAndDisabledAreNoops(t *testing.T) {
	// nil gate.
	var nilGate *upstreamThrottle
	release, err := nilGate.acquire(context.Background(), upstreamJira)
	if err != nil {
		t.Fatalf("nil gate acquire: %v", err)
	}
	release()

	// Disabled gate (limit <= 0).
	off := newUpstreamThrottle(0)
	release, err = off.acquire(context.Background(), upstreamGitHub)
	if err != nil {
		t.Fatalf("disabled gate acquire: %v", err)
	}
	release()
}

func TestUpstreamThrottle_BoundsConcurrencyPerUpstream(t *testing.T) {
	const limit = 2
	gate := newUpstreamThrottle(limit)

	// Fill both Jira slots.
	r1, err := gate.acquire(context.Background(), upstreamJira)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := gate.acquire(context.Background(), upstreamJira)
	if err != nil {
		t.Fatal(err)
	}

	// A third Jira acquire must block until a slot frees.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.acquire(ctx, upstreamJira); err == nil {
		t.Fatal("third Jira acquire should have blocked past the deadline")
	}

	// GitHub is a separate pool — not starved by the full Jira pool.
	rgh, err := gate.acquire(context.Background(), upstreamGitHub)
	if err != nil {
		t.Fatalf("github acquire should not be blocked by jira: %v", err)
	}
	rgh()

	// Free one Jira slot; a new Jira acquire now succeeds promptly.
	r1()
	done := make(chan struct{})
	go func() {
		r, err := gate.acquire(context.Background(), upstreamJira)
		if err == nil {
			r()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Jira acquire did not proceed after a slot freed")
	}
	r2()
}

// TestUpstreamThrottle_ConcurrentAcquireNeverExceedsLimit hammers one pool from
// many goroutines and asserts the live-holder count never crosses the cap.
func TestUpstreamThrottle_ConcurrentAcquireNeverExceedsLimit(t *testing.T) {
	const limit = 4
	gate := newUpstreamThrottle(limit)

	var mu sync.Mutex
	live, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := gate.acquire(context.Background(), upstreamGitHub)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			live++
			if live > peak {
				peak = live
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			live--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	if peak > limit {
		t.Errorf("peak concurrency = %d, exceeds limit %d", peak, limit)
	}
}
