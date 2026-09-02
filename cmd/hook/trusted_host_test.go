package hook

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// TestIsTrustedHost_EmptyBaseIsTheDeploymentDefault pins the fallback gate:
// with no org host resolved, a push is recorded only when it goes to the
// deployment's default GitHub — github.com with the variable unset, and the
// configured host otherwise, where github.com is then a stranger.
func TestIsTrustedHost_EmptyBaseIsTheDeploymentDefault(t *testing.T) {
	ghbase.SetDefaultBaseURLForTest(t, "")
	if !isTrustedHost("github.com", "") {
		t.Error("github.com not trusted with the default unset")
	}
	if isTrustedHost("ghe.example.com", "") {
		t.Error("a GHES host trusted with the default unset")
	}

	ghbase.SetDefaultBaseURLForTest(t, "https://ghe.example.com")
	if !isTrustedHost("ghe.example.com:443", "") {
		t.Error("the deployment default not trusted when it is the configured host")
	}
	if isTrustedHost("github.com", "") {
		t.Error("github.com trusted under a GHES default — a push there is another GitHub's")
	}
	// An org host, when resolved, still wins over the default.
	if !isTrustedHost("git.corp.example.com", "git.corp.example.com") || isTrustedHost("ghe.example.com", "git.corp.example.com") {
		t.Error("a resolved org host must decide alone")
	}
}
