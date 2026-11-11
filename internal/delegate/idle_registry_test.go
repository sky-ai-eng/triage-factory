package delegate

import "testing"

func received(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestIdleRegistry_LetGoOldestTakesTheLongestIdle(t *testing.T) {
	r := NewIdleRegistry()
	first := r.Register("conv-1", "org-a")
	second := r.Register("conv-2", "org-a")

	if !r.LetGoOldest() {
		t.Fatal("LetGoOldest = false with two engagements waiting")
	}
	if !received(first.LetGo()) {
		t.Error("the engagement idle longest was not let go")
	}
	if received(second.LetGo()) {
		t.Error("the newer engagement was let go")
	}
	if r.Wake("conv-1") {
		t.Error("the let-go engagement is still registered")
	}
	if r.Len() != 1 {
		t.Errorf("Len after one let-go = %d, want 1", r.Len())
	}
	if !r.LetGoOldest() || !received(second.LetGo()) {
		t.Error("the second LetGoOldest did not let the remaining engagement go")
	}
	if r.LetGoOldest() {
		t.Error("LetGoOldest = true on an empty registry")
	}
}

func TestIdleRegistry_LetGoOldestInOrgStaysInTheOrg(t *testing.T) {
	r := NewIdleRegistry()
	otherOrg := r.Register("conv-1", "org-a")
	mine := r.Register("conv-2", "org-b")

	if !r.LetGoOldestInOrg("org-b") {
		t.Fatal("LetGoOldestInOrg(org-b) = false with an org-b engagement waiting")
	}
	if !received(mine.LetGo()) {
		t.Error("the org's engagement was not let go")
	}
	if received(otherOrg.LetGo()) {
		t.Error("an older engagement in another org was let go")
	}
	if r.LetGoOldestInOrg("org-b") {
		t.Error("LetGoOldestInOrg(org-b) = true with none of the org's engagements left")
	}
	r.LetGoAll()
	if !received(otherOrg.LetGo()) || r.Len() != 0 {
		t.Errorf("LetGoAll left Len %d and the remaining engagement unsignalled", r.Len())
	}
}

func TestIdleRegistry_RingAfterUnregisterIsANoOp(t *testing.T) {
	r := NewIdleRegistry()
	w := r.Register("conv-1", "org-a")
	w.Unregister()
	w.Unregister()

	if r.Wake("conv-1") {
		t.Error("Wake reported an engagement that unregistered")
	}
	if r.LetGoOldest() {
		t.Error("LetGoOldest found an engagement that unregistered")
	}
	if received(w.Wake()) || received(w.LetGo()) {
		t.Error("a ring reached an engagement after it unregistered")
	}
}

func TestIdleRegistry_ADoubleRingDoesNotBlock(t *testing.T) {
	r := NewIdleRegistry()
	w := r.Register("conv-1", "org-a")

	for range 2 {
		if !r.Wake("conv-1") {
			t.Fatal("Wake = false for a registered engagement")
		}
	}
	if !received(w.Wake()) {
		t.Fatal("the wake channel holds no ring")
	}
	if received(w.Wake()) {
		t.Error("two rings left two signals; the second is the same news")
	}
}
