//go:build linux

package sandbox

import "sync"

// keyedMutex serializes work on one key without serializing work on others.
// Entries are counted and dropped when the last holder lets go, so the map
// holds only the keys in use: run-tree roots and conversation ids come and go
// for the life of the broker. The zero value is ready to use.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

type refMutex struct {
	mu   sync.Mutex
	refs int
}

// lock takes key's mutex and returns the func that releases it, which the
// caller invokes exactly once.
func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*refMutex{}
	}
	rm := k.locks[key]
	if rm == nil {
		rm = &refMutex{}
		k.locks[key] = rm
	}
	rm.refs++
	k.mu.Unlock()

	rm.mu.Lock()
	return func() {
		rm.mu.Unlock()
		k.mu.Lock()
		rm.refs--
		if rm.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// runTreeOwnership serializes, per root, every change of a run tree's owner:
// a hand-off and a park of the same tree run one at a time. The broker serves
// its callers concurrently, and a park reads the root's owner before it
// changes it, so a successor's hand-off landing between the two would
// otherwise be parked out from under the successor.
var runTreeOwnership keyedMutex

// runSocketDirs serializes, per conversation, the creation and removal of a
// run's socket directories, for the same reason: ReleaseRunSockets checks a
// directory's owner before removing it, and a successor's PrepareRunSockets
// replacing that directory in between would lose its live one.
var runSocketDirs keyedMutex

// stagingGrants serializes, per directory, granting a launch's staging to a
// jail and taking that grant back. Both run in the orchestrator (RunNetwork's
// GrantStaging and Close), and the revoke reads the directory's group before
// it changes it, so a successor's grant landing between the two would
// otherwise be taken away.
var stagingGrants keyedMutex
