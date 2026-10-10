package credmiss

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
)

func TestStatus(t *testing.T) {
	want := map[string]int{
		credbundle.MissNoBundle:           http.StatusBadGateway,
		credbundle.MissTokenExpiring:      http.StatusBadGateway,
		credbundle.MissOther:              http.StatusBadGateway,
		credbundle.MissNoRepoToken:        http.StatusForbidden,
		credbundle.MissNoCLIToken:         http.StatusForbidden,
		credbundle.MissNoJiraCredential:   http.StatusForbidden,
		credbundle.MissNoLinearCredential: http.StatusForbidden,
	}
	for reason, status := range want {
		if got := Status(reason); got != status {
			t.Errorf("Status(%q) = %d, want %d", reason, got, status)
		}
	}
}

func TestRespond(t *testing.T) {
	var buf bytes.Buffer
	r := NewResponder("testproxy", "conv-1", slog.New(slog.NewTextHandler(&buf, nil)))

	// The error text stands in for the App or credential-setup detail a real
	// source error can carry, and the repository for tenant data.
	err := fmt.Errorf("mint for app 4242, repo acme/secret-widgets: %w", credbundle.ErrNoRepoToken)
	for range 3 {
		rec := httptest.NewRecorder()
		r.Respond(rec, err)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]any
		if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
			t.Fatalf("body is not a JSON object: %v (%q)", jerr, rec.Body.String())
		}
		msg, _ := body["message"].(string)
		if want := "testproxy: no credential for this request (no_repo_token)"; msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
		for _, leak := range []string{"4242", "secret-widgets"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("body leaks %q: %s", leak, rec.Body.String())
			}
		}
	}

	r.Respond(httptest.NewRecorder(), credbundle.ErrNoBundle)
	r.Respond(httptest.NewRecorder(), credbundle.ErrNoBundle)

	logged := buf.String()
	if n := strings.Count(logged, "credential lookup failed"); n != 2 {
		t.Fatalf("logged %d lines, want one per distinct reason (2):\n%s", n, logged)
	}
	for _, want := range []string{"level=WARN", "proxy=testproxy", "conversation=conv-1", "reason=no_repo_token", "reason=no_bundle"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
	for _, leak := range []string{"4242", "secret-widgets", "acme"} {
		if strings.Contains(logged, leak) {
			t.Errorf("log leaks %q:\n%s", leak, logged)
		}
	}
}
