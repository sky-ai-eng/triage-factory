package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSeatLimit_EndToEnd_HardBlock drives the real multi-mode OAuth callback with
// a committed 1-seat cap and pins the whole contract end-to-end: the first
// distinct user is admitted (session minted, login_success recorded); a NEW
// distinct user beyond the cap is hard-blocked (302 to /login?error=seat_limit,
// NO session cookie, a seat_limit_rejected row, and crucially NO login_success —
// so a blocked login never consumes a seat); and a returning, already-active user
// is still admitted despite the deployment sitting at cap. pgtest-backed, so it
// skips cleanly without Docker.
func TestSeatLimit_EndToEnd_HardBlock(t *testing.T) {
	r := newAuthRig(t)
	setSeatCap(t, 1) // one committed seat for the whole deployment

	// User A — first login this period → admitted.
	userA := r.seedUser()
	respA, _ := r.driveCallback(userA)
	if respA.StatusCode != http.StatusFound {
		t.Fatalf("A login status=%d, want 302", respA.StatusCode)
	}
	_ = r.sidFromResp(respA) // fatal if A got no session cookie
	if got := r.authEventsByType(domain.AuthEventLoginSuccess); len(got) != 1 || got[0].userID != userA.String() {
		t.Fatalf("A should record exactly one login_success; got %v", got)
	}

	// User B — a new distinct user beyond the cap → hard-blocked.
	userB := r.seedUser()
	respB, _ := r.driveCallback(userB)
	if respB.StatusCode != http.StatusFound {
		t.Fatalf("B login status=%d, want 302", respB.StatusCode)
	}
	loc := respB.Header.Get("Location")
	if !strings.Contains(loc, "/login") || !strings.Contains(loc, "error=seat_limit") {
		t.Fatalf("B redirect=%q, want /login?error=seat_limit", loc)
	}
	for _, c := range respB.Cookies() {
		if c.Name == r.srv.sidCookieName() {
			t.Fatal("a blocked login must not mint a session cookie")
		}
	}
	rej := r.authEventsByType(domain.AuthEventSeatLimitRejected)
	if len(rej) != 1 || rej[0].userID != userB.String() {
		t.Fatalf("expected one seat_limit_rejected for B; got %v", rej)
	}
	if rej[0].detail["max_seats"] != float64(1) {
		t.Errorf("rejection detail max_seats=%v, want 1", rej[0].detail["max_seats"])
	}
	// The blocked login recorded NO login_success, so B never consumed a seat.
	if got := r.authEventsByType(domain.AuthEventLoginSuccess); len(got) != 1 {
		t.Fatalf("B must not record login_success; login_success count=%d, want 1 (A only)", len(got))
	}

	// User A again — returning + already active this period → still admitted even
	// though the deployment is at its 1-seat cap.
	respA2, _ := r.driveCallback(userA)
	if respA2.StatusCode != http.StatusFound {
		t.Fatalf("A re-login status=%d, want 302", respA2.StatusCode)
	}
	_ = r.sidFromResp(respA2) // fatal if the returning user was wrongly bounced
}
