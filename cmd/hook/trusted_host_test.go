package hook

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// TestIsTrustedHost_EmptyBaseTrustsNoPublicHost pins the gate with no org host
// resolved: a push can be the org's only if the org names its GitHub, so no
// public host is trusted, the deployment's default GitHub included. A private
// or loopback address (the sandbox git proxy's veth) is still trusted.
func TestIsTrustedHost_EmptyBaseTrustsNoPublicHost(t *testing.T) {
	ghbase.SetDefaultBaseURLForTest(t, "")
	if isTrustedHost("github.com", "") {
		t.Error("github.com trusted with no org host")
	}
	ghbase.SetDefaultBaseURLForTest(t, "https://ghe.example.com")
	if isTrustedHost("ghe.example.com:443", "") {
		t.Error("the deployment default trusted with no org host")
	}
	if !isTrustedHost("10.42.0.1:38573", "") {
		t.Error("the sandbox proxy's private address not trusted")
	}
	// An org host, when resolved, decides alone.
	if !isTrustedHost("git.corp.example.com", "git.corp.example.com") || isTrustedHost("ghe.example.com", "git.corp.example.com") {
		t.Error("a resolved org host must decide alone")
	}
}
