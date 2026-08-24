package gitproxy_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// TestGatedProxy_AuthorizeErrorIsLogged pins the one place a gate error's cause
// survives. The 502 body deliberately tells the agent nothing about server
// internals and git prints that body as its entire explanation; the audit
// record's Reason is a fixed vocabulary with no room for an error string. So
// without this line the operator-visible account of a run that cannot touch git
// at all is "authorization check failed", with no way to tell a broken store
// read from a relay outage.
func TestGatedProxy_AuthorizeErrorIsLogged(t *testing.T) {
	var buf bytes.Buffer
	defer logging.SetOutput(&buf)()

	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	errAuthorize := func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
		return gitproxy.Decision{}, errors.New("push policy read: column \"base_branch_push_policy\" does not exist")
	}
	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, errAuthorize, sink.record)

	req, _ := http.NewRequest("GET", proxyURL+"/octo/repo/info/refs", nil)
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	got := buf.String()
	// The cause verbatim, and the repo it was refused for — an operator reading
	// this needs to know which repo stalled as much as why.
	for _, want := range []string{"base_branch_push_policy", "push policy read", "octo", "repo"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q; got:\n%s", want, got)
		}
	}
}
