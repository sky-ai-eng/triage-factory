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

func TestWarmWaits_LetGoOldestTakesTheLongestWait(t *testing.T) {
	r := NewWarmWaits()
	first := r.Register("conv-1", "org-a")
	second := r.Register("conv-2", "org-a")

	if _, ok := r.LetGoOldest(); !ok {
		t.Fatal("LetGoOldest = false with two engagements waiting")
	}
	if !received(first.LetGo()) {
		t.Error("the engagement waiting longest was not let go")
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
	if _, ok := r.LetGoOldest(); !ok || !received(second.LetGo()) {
		t.Error("the second LetGoOldest did not let the remaining engagement go")
	}
	if _, ok := r.LetGoOldest(); ok {
		t.Error("LetGoOldest = true on an empty registry")
	}
}

func TestWarmWaits_LetGoOldestInOrgStaysInTheOrg(t *testing.T) {
	r := NewWarmWaits()
	otherOrg := r.Register("conv-1", "org-a")
	mine := r.Register("conv-2", "org-b")

	if _, ok := r.LetGoOldestInOrg("org-b"); !ok {
		t.Fatal("LetGoOldestInOrg(org-b) = false with an org-b engagement waiting")
	}
	if !received(mine.LetGo()) {
		t.Error("the org's engagement was not let go")
	}
	if received(otherOrg.LetGo()) {
		t.Error("an older engagement in another org was let go")
	}
	if _, ok := r.LetGoOldestInOrg("org-b"); ok {
		t.Error("LetGoOldestInOrg(org-b) = true with none of the org's engagements left")
	}
	r.LetGoAll()
	if !received(otherOrg.LetGo()) || r.Len() != 0 {
		t.Errorf("LetGoAll left Len %d and the remaining engagement unsignalled", r.Len())
	}
}

func TestWarmWaits_RingAfterUnregisterIsANoOp(t *testing.T) {
	r := NewWarmWaits()
	w := r.Register("conv-1", "org-a")
	w.Unregister()
	w.Unregister()

	if r.Wake("conv-1") {
		t.Error("Wake reported an engagement that unregistered")
	}
	if _, ok := r.LetGoOldest(); ok {
		t.Error("LetGoOldest found an engagement that unregistered")
	}
	if received(w.Wake()) || received(w.LetGo()) {
		t.Error("a ring reached an engagement after it unregistered")
	}
}

func TestWarmWaits_ADoubleRingDoesNotBlock(t *testing.T) {
	r := NewWarmWaits()
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

func TestWarmWaits_OrgsWaitingNamesEachOrgOnce(t *testing.T) {
	r := NewWarmWaits()
	if orgs := r.OrgsWaiting(); len(orgs) != 0 {
		t.Fatalf("OrgsWaiting on an empty registry = %v, want none", orgs)
	}
	r.Register("conv-1", "org-b")
	r.Register("conv-2", "org-a")
	gone := r.Register("conv-3", "org-c")
	r.Register("conv-4", "org-b")
	gone.Unregister()

	got := r.OrgsWaiting()
	want := []string{"org-a", "org-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("OrgsWaiting = %v, want %v", got, want)
	}
}

// A let-go and a resume decided at once settle under the registry's lock:
// whichever comes first stands, and the other finds nothing.
func TestWarmWaits_TakeForResumeSettlesAgainstALetGo(t *testing.T) {
	r := NewWarmWaits()
	letGoFirst := r.Register("conv-1", "org-a")
	if _, ok := r.LetGoOldest(); !ok {
		t.Fatal("LetGoOldest found no engagement")
	}
	if letGoFirst.TakeForResume() {
		t.Error("TakeForResume took an engagement the registry had already let go")
	}

	resumeFirst := r.Register("conv-2", "org-a")
	if !resumeFirst.TakeForResume() {
		t.Fatal("TakeForResume refused an engagement nothing had let go")
	}
	if _, ok := r.LetGoOldest(); ok {
		t.Error("LetGoOldest let go of an engagement that had already resumed")
	}
	if received(resumeFirst.LetGo()) {
		t.Error("the resumed engagement was rung to let go")
	}
}
