package events

import "testing"

// TestAdditiveFor pins the lookup: registered Additive=true reports true,
// an unregistered type (or one registered without setting Additive) falls
// to the safe false default — deferral, never a silent drop.
func TestAdditiveFor(t *testing.T) {
	const additiveType = "fake:additive_for_test:additive"
	const nonAdditiveType = "fake:additive_for_test:non_additive"

	Register(EventSchema{
		EventType: additiveType,
		Ownership: OwnershipOwned,
		Additive:  true,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
	t.Cleanup(func() { Reset(additiveType) })

	Register(EventSchema{
		EventType: nonAdditiveType,
		Ownership: OwnershipOwned,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
	t.Cleanup(func() { Reset(nonAdditiveType) })

	if !AdditiveFor(additiveType) {
		t.Errorf("AdditiveFor(%q) = false, want true", additiveType)
	}
	if AdditiveFor(nonAdditiveType) {
		t.Errorf("AdditiveFor(%q) = true, want false (schema didn't set Additive)", nonAdditiveType)
	}
	if AdditiveFor("some:unregistered:event") {
		t.Error("AdditiveFor on an unregistered type = true, want false (safe default)")
	}
}

// TestReset_RemovesRegistration pins the test-cleanup helper: after Reset,
// the type is unregistered (Get reports ok=false) and can be re-registered
// without tripping Register's duplicate panic.
func TestReset_RemovesRegistration(t *testing.T) {
	const eventType = "fake:reset_test:thing"

	Register(EventSchema{
		EventType: eventType,
		Ownership: OwnershipPool,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
	if _, ok := Get(eventType); !ok {
		t.Fatal("expected the type to be registered")
	}

	Reset(eventType)
	if _, ok := Get(eventType); ok {
		t.Error("expected Reset to remove the registration")
	}

	// Must not panic — Reset cleared the slot.
	Register(EventSchema{
		EventType: eventType,
		Ownership: OwnershipPool,
		Match:     func(string, string) (bool, error) { return true, nil },
	})
	t.Cleanup(func() { Reset(eventType) })
}
