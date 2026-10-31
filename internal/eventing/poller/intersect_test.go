package poller

import (
	"testing"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// TestIntersectConfigured pins the App-path repo mapping: only configured
// repos present in an installation's grant are polled, matching is
// case-insensitive on the "owner/repo" slug, and every matched repo is
// marked in `covered` so the caller can report configured repos that no
// installation grants.
func TestIntersectConfigured(t *testing.T) {
	configured := []string{"octo/engineering", "octo/marketing", "octo/secret"}
	grant := []ghclient.UserRepo{
		{FullName: "Octo/Engineering"}, // different case — must still match
		{FullName: "octo/marketing"},
		{FullName: "octo/unconfigured"}, // granted but not configured — ignored
	}

	covered := map[string]bool{}
	got := intersectConfigured(configured, grant, covered)

	want := map[string]bool{"octo/engineering": true, "octo/marketing": true}
	if len(got) != len(want) {
		t.Fatalf("intersection = %v; want keys %v", got, want)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected repo %q in intersection", r)
		}
	}

	// covered must mark exactly the matched configured repos (by their
	// configured spelling), leaving the unreachable one uncovered.
	if !covered["octo/engineering"] || !covered["octo/marketing"] {
		t.Errorf("covered missing matched repos: %v", covered)
	}
	if covered["octo/secret"] {
		t.Errorf("octo/secret has no grant but was marked covered")
	}
}

// TestIntersectConfigured_AccumulatesAcrossInstallations pins that `covered`
// accumulates across calls — each installation contributes its reachable
// subset, and a repo reachable by any installation ends up covered.
func TestIntersectConfigured_AccumulatesAcrossInstallations(t *testing.T) {
	configured := []string{"octo/a", "octo/b", "octo/c"}
	covered := map[string]bool{}

	intersectConfigured(configured, []ghclient.UserRepo{{FullName: "octo/a"}}, covered)
	intersectConfigured(configured, []ghclient.UserRepo{{FullName: "octo/b"}}, covered)

	if !covered["octo/a"] || !covered["octo/b"] {
		t.Errorf("covered should accumulate octo/a + octo/b: %v", covered)
	}
	if covered["octo/c"] {
		t.Errorf("octo/c reachable by no installation; should be uncovered: %v", covered)
	}
}
