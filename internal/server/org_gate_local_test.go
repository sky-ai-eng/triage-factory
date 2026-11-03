package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// TestOrgGates_Local: local mode's org gates answer the local org in any
// spelling uuid.Parse accepts, and answer any other org 404, the answer multi
// gives an org the caller is not in. One member-gated route and one admin-gated
// route, both of which hand the gate's org id to the store.
func TestOrgGates_Local(t *testing.T) {
	s := newTestServer(t)
	unconfigureEventSources(t)

	routes := []struct {
		name   string
		method string
		path   string // with %ORG% for the org id
		body   any
	}{
		{"member-gated settings read", http.MethodGet, "/api/orgs/%ORG%/settings", nil},
		{"admin-gated source write", http.MethodPatch, "/api/orgs/%ORG%/sources/jira", map[string]any{"disabled": false}},
	}
	orgs := []struct {
		name string
		org  string
		want int
	}{
		{"the local org", runmode.LocalDefaultOrgID, http.StatusOK},
		{"the local org without hyphens", strings.ReplaceAll(runmode.LocalDefaultOrgID, "-", ""), http.StatusOK},
		{"the local org upper-cased", strings.ToUpper(runmode.LocalDefaultOrgID), http.StatusOK},
		{"another org", "00000000-0000-0000-0000-000000000099", http.StatusNotFound},
	}
	for _, rt := range routes {
		for _, o := range orgs {
			t.Run(rt.name+"/"+o.name, func(t *testing.T) {
				path := strings.ReplaceAll(rt.path, "%ORG%", o.org)
				rec := doJSON(t, s, rt.method, path, rt.body)
				if rec.Code != o.want {
					t.Fatalf("%s %s = %d, want %d; body=%s", rt.method, path, rec.Code, o.want, rec.Body.String())
				}
				if o.want == http.StatusNotFound {
					httpxtest.AssertFirstError(t, rec, httpx.ReasonNotFound, "")
				}
			})
		}
	}
}
