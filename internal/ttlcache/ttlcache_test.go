package ttlcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func counting(n *atomic.Int32, v string, keep bool, err error) func(context.Context) (string, bool, error) {
	return func(context.Context) (string, bool, error) {
		n.Add(1)
		return v, keep, err
	}
}

func TestGet_KeepsAValueForItsTTL(t *testing.T) {
	c := New[string](time.Minute)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	var loads atomic.Int32

	for range 3 {
		v, err := c.Get(context.Background(), "k", counting(&loads, "a", true, nil))
		if err != nil || v != "a" {
			t.Fatalf("Get = %q, %v", v, err)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads within the TTL = %d, want 1", loads.Load())
	}

	now = now.Add(time.Minute)
	if _, err := c.Get(context.Background(), "k", counting(&loads, "b", true, nil)); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 2 {
		t.Fatalf("loads after the TTL = %d, want 2", loads.Load())
	}
}

func TestGet_DoesNotKeepAPartialAnswerOrAnError(t *testing.T) {
	c := New[string](time.Minute)
	var loads atomic.Int32

	if v, _ := c.Get(context.Background(), "k", counting(&loads, "partial", false, nil)); v != "partial" {
		t.Fatalf("partial answer not returned: %q", v)
	}
	if _, err := c.Get(context.Background(), "k", counting(&loads, "", true, errors.New("down"))); err == nil {
		t.Fatal("error not returned")
	}
	if _, err := c.Get(context.Background(), "k", counting(&loads, "whole", true, nil)); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 3 {
		t.Fatalf("loads = %d, want 3: neither the partial answer nor the error may be kept", loads.Load())
	}
}

func TestGet_SharesOneLoadAcrossConcurrentMisses(t *testing.T) {
	c := New[string](time.Minute)
	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (string, bool, error) {
		loads.Add(1)
		<-release
		return "a", true, nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Get(context.Background(), "k", load)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}

func TestForget_DropsTheValueAndAnInFlightLoad(t *testing.T) {
	c := New[string](time.Minute)
	var loads atomic.Int32
	if _, err := c.Get(context.Background(), "k", counting(&loads, "old", true, nil)); err != nil {
		t.Fatal(err)
	}
	c.Forget("k")
	if v, _ := c.Get(context.Background(), "k", counting(&loads, "new", true, nil)); v != "new" {
		t.Fatalf("after Forget = %q, want a fresh load", v)
	}

	// A load that began before a Forget answers its callers but is not kept.
	started, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = c.Get(context.Background(), "j", func(context.Context) (string, bool, error) {
			close(started)
			<-release
			return "stale", true, nil
		})
	}()
	<-started
	c.Forget("j")
	close(release)
	time.Sleep(20 * time.Millisecond)
	if v, _ := c.Get(context.Background(), "j", counting(&loads, "fresh", true, nil)); v != "fresh" {
		t.Fatalf("Get after a Forget during a load = %q, want a fresh load", v)
	}
}

func TestNilCache_LoadsEveryTime(t *testing.T) {
	var c *Cache[string]
	var loads atomic.Int32
	for range 2 {
		if _, err := c.Get(context.Background(), "k", counting(&loads, "a", true, nil)); err != nil {
			t.Fatal(err)
		}
	}
	c.Forget("k")
	if loads.Load() != 2 {
		t.Fatalf("loads = %d, want 2", loads.Load())
	}
}
