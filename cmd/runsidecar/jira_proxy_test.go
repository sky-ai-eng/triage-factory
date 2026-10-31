package runsidecar

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRuntime_JiraAuthHeaderReadsTheHeldBundle pins that the Jira proxy's auth
// is read per request: a re-sealed credential is used from the next request,
// a re-seal whose manifest no longer names Jira sealed answers with that
// state, and a credential resolved for another Jira site is never sent to the
// one this proxy forwards to.
func TestRuntime_JiraAuthHeaderReadsTheHeldBundle(t *testing.T) {
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	ctx := context.Background()
	const site = "https://jira.acme.internal"
	rt.jiraBase = site
	hold := func(jc *credbundle.JiraCreds, state domain.SourceState) {
		rt.mu.Lock()
		rt.bundle = &credbundle.Bundle{Jira: jc, Sources: domain.SourceManifest{"jira": {State: state}}}
		rt.mu.Unlock()
	}

	hold(&credbundle.JiraCreds{URL: site, AuthMethod: "datacenter", PAT: "pat-1"}, domain.SourceSealed)
	if got, err := rt.jiraAuthHeader(ctx); err != nil || got != "Bearer pat-1" {
		t.Fatalf("first bundle = (%q, %v), want Bearer pat-1", got, err)
	}
	hold(&credbundle.JiraCreds{URL: site, AuthMethod: "datacenter", PAT: "pat-2"}, domain.SourceSealed)
	if got, err := rt.jiraAuthHeader(ctx); err != nil || got != "Bearer pat-2" {
		t.Fatalf("re-sealed bundle = (%q, %v), want Bearer pat-2", got, err)
	}
	hold(&credbundle.JiraCreds{URL: site, AuthMethod: "cloud", Email: "bot@acme.com", APIToken: "tok"}, domain.SourceSealed)
	if got, err := rt.jiraAuthHeader(ctx); err != nil || !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("cloud bundle = (%q, %v), want Basic auth", got, err)
	}

	hold(nil, domain.SourceDisabled)
	var miss *credbundle.SourceMissError
	if _, err := rt.jiraAuthHeader(ctx); !errors.As(err, &miss) || miss.Kind != "jira" || miss.State != domain.SourceDisabled {
		t.Errorf("turned off mid-run: error %v, want a disabled Jira miss", err)
	}

	hold(&credbundle.JiraCreds{URL: "https://other.atlassian.net", AuthMethod: "datacenter", PAT: "pat-3"}, domain.SourceSealed)
	if got, err := rt.jiraAuthHeader(ctx); err == nil {
		t.Errorf("a credential for another Jira site was answered as %q", got)
	}
}
