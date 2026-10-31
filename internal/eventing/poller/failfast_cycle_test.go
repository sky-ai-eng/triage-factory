package poller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// attemptCounter counts the requests that reach the network.
type attemptCounter struct {
	n    atomic.Int32
	base http.RoundTripper
}

func (c *attemptCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(r)
}

// TestGitHubCycle_UnreachableHostCostsOneRetryLadder: an org tracking many
// repos on a host that refuses connections. The first listing spends the
// client's whole retry ladder learning the host is down; every other listing
// in the cycle is sent once. Without that, the cycle takes one ladder per
// repo, and every org polled after it waits that long.
func TestGitHubCycle_UnreachableHostCostsOneRetryLadder(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	const repoCount = 16
	repos := make([]string, repoCount)
	modes := map[string]string{}
	for i := range repos {
		name := fmt.Sprintf("r%02d", i)
		repos[i] = "octo/" + name
		modes[name] = listOK
	}
	f := newConnectionFixture(t, newGitHubConnServer(t, modes), repos...)
	// A real ladder, so a cycle that waits one per repo is visibly slower.
	ghclient.SetTransientBackoffForTest(t, 20*time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refused := "http://" + ln.Addr().String()
	_ = ln.Close()
	counter := &attemptCounter{base: &http.Transport{}}
	f.m.resolver = &fakeResolver{client: ghclient.NewClientWithHTTPClient(refused, "pat", &http.Client{Transport: counter})}

	start := time.Now()
	f.m.runGitHubCycleForOrg(context.Background(), f.org)
	elapsed := time.Since(start)

	if st := f.connection(t); st.State != dbpkg.ConnectionDown || st.FailureClass != string(upstream.Transient) {
		t.Errorf("state = %+v, want down/transient", st)
	}
	// One full ladder (4 attempts) for the first listing, one attempt for each
	// other repo.
	if got, want := int(counter.n.Load()), 4+(repoCount-1); got != want {
		t.Errorf("the cycle made %d attempts against the refused host, want %d (one ladder, then one per repo)", got, want)
	}
	// One ladder is 20+40+80ms; one per repo would be over 2s.
	if elapsed > time.Second {
		t.Errorf("the cycle took %v against a refused host", elapsed)
	}
}
