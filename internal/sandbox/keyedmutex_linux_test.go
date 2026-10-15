//go:build linux

package sandbox

import (
	"testing"
	"time"
)

// TestKeyedMutex pins the two properties the run-tree and socket locks rest
// on: a key waits for its holder, another key does not, and nothing is kept
// for a key once its last holder lets go.
func TestKeyedMutex(t *testing.T) {
	var k keyedMutex
	unlockA := k.lock("a")

	other := make(chan struct{})
	go func() {
		k.lock("b")()
		close(other)
	}()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("a lock on another key waited for the holder of this one")
	}

	same := make(chan struct{})
	go func() {
		k.lock("a")()
		close(same)
	}()
	select {
	case <-same:
		t.Fatal("a second lock on a held key did not wait for its holder")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-same:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting lock was not granted when the holder let go")
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.locks) != 0 {
		t.Errorf("%d keys still held after every holder let go", len(k.locks))
	}
}
