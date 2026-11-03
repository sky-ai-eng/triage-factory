// Package ttlcache holds values read from an upstream for a fixed time, so a
// list route paging or searching one enumeration (a Slack workspace's
// channels, a GitHub org's teams) reads the upstream once per window rather
// than once per request.
package ttlcache

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Cache holds one value per key for its TTL. It is per process: in multi mode
// each control pod holds its own, which bounds how stale an answer is (one
// TTL) without any cross-pod invalidation.
//
// A nil *Cache caches nothing: Get calls load every time. That keeps a value
// built without one (a test fixture) correct rather than panicking.
type Cache[V any] struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]entry[V]
	// gens counts each key's Forgets, so a load that began before one does
	// not store the answer the Forget was meant to drop.
	gens   map[string]uint64
	flight singleflight.Group
}

type entry[V any] struct {
	value   V
	expires time.Time
}

// New returns a cache whose values live for ttl.
func New[V any](ttl time.Duration) *Cache[V] {
	return &Cache[V]{ttl: ttl, now: time.Now, entries: map[string]entry[V]{}, gens: map[string]uint64{}}
}

// Get returns key's value, calling load when the cache holds none or the one
// it holds has outlived the TTL. Concurrent misses for one key share a single
// load. load reports whether its value may be kept: a partial answer (an
// upstream that failed for some of what was asked) is returned to the callers
// waiting on it but not kept, so the next Get asks again. An error is never
// kept either.
//
// load runs under a context that ignores the caller's cancellation, since
// other callers may be waiting on the same load; the upstream client's own
// timeouts bound it.
func (c *Cache[V]) Get(ctx context.Context, key string, load func(context.Context) (value V, keep bool, err error)) (V, error) {
	if c == nil {
		v, _, err := load(ctx)
		return v, err
	}
	if v, ok := c.lookup(key); ok {
		return v, nil
	}
	got, err, _ := c.flight.Do(key, func() (any, error) {
		gen := c.generation(key)
		v, keep, err := load(context.WithoutCancel(ctx))
		if err == nil && keep {
			c.store(key, gen, v)
		}
		return v, err
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return got.(V), nil
}

// Forget drops key's value, so the next Get loads it. A write that changes
// what the upstream answers calls it, so its own response and the reads after
// it see the change rather than the value held from before.
func (c *Cache[V]) Forget(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.gens[key]++
	c.mu.Unlock()
	c.flight.Forget(key)
}

func (c *Cache[V]) generation(key string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens[key]
}

func (c *Cache[V]) lookup(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// store keeps v unless key was forgotten since its load began (gen), and
// drops every entry whose time has passed, so the map holds at most one TTL's
// worth of keys.
func (c *Cache[V]) store(key string, gen uint64, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gens[key] != gen {
		return
	}
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = entry[V]{value: v, expires: now.Add(c.ttl)}
}
