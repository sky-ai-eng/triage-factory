package dnsoverride

import (
	"context"
	"testing"
)

type fakeA struct{}

func (fakeA) LookupTXT(context.Context, string) ([]string, error) { return nil, nil }

type fakeB struct{}

func (fakeB) LookupTXT(context.Context, string) ([]string, error) { return []string{"b"}, nil }

// Guards the atomic.Value pitfalls: storing nil panics, and storing two
// different concrete types over the value's lifetime panics. Both used to be
// live (Set(nil) cleared via a nil interface; the resolver's dynamic type
// varied per test). The resolverHolder wrapper must make all three calls below
// safe.
func TestSetGetClear_NoPanic(t *testing.T) {
	t.Cleanup(func() { Set(nil) })

	Set(fakeA{})
	if _, ok := Get().(fakeA); !ok {
		t.Fatalf("Get() = %T, want fakeA after Set(fakeA{})", Get())
	}

	// A different concrete type must not panic and must replace the override.
	Set(fakeB{})
	if _, ok := Get().(fakeB); !ok {
		t.Fatalf("Get() = %T, want fakeB after Set(fakeB{})", Get())
	}

	// Clearing with nil must not panic and Get must report no override.
	Set(nil)
	if got := Get(); got != nil {
		t.Fatalf("Get() = %v, want nil after Set(nil)", got)
	}
}
