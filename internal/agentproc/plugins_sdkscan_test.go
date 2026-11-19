package agentproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// documentedPluginEnvAllowPrefixes are the TF_PLUGIN_ENV_ALLOW_PREFIXES values
// the docs and tests name. Each must cover no key the pinned SDK reads, or an
// operator following the docs would hand a team a key Claude Code acts on.
var documentedPluginEnvAllowPrefixes = []string{"CLAUDE_LOOP_HARNESS_"}

// TestSDKKeyScan_AllowedPrefixesReadNoKey installs the pinned SDK from the
// embedded package.json and lockfile, scans its JavaScript and the platform
// Claude Code binary for CLAUDE_* keys, and fails when a documented allowed
// prefix covers one. An SDK bump that starts reading such a key fails here.
//
// It needs node and network, so it runs only when asked, which the CI job
// does on every change that can move the SDK. Once asked it never skips: an
// install or scan that cannot complete is a failure.
//
//	TF_TEST_SDK_KEY_SCAN=1 go test ./internal/agentproc -run TestSDKKeyScan -v
func TestSDKKeyScan_AllowedPrefixesReadNoKey(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_KEY_SCAN") != "1" {
		t.Skip("set TF_TEST_SDK_KEY_SCAN=1 to install the pinned SDK and scan it")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Fatalf("npm is required for the SDK key scan: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), sdkPackageJSON(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), packageLockJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	install := exec.Command(npm, "ci", "--no-audit", "--no-fund")
	install.Dir = dir
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("npm ci: %v\n%s", err, out)
	}

	keys, err := ScanSDKClaudeKeys(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// A scan that found almost nothing read the wrong files; Claude Code
	// reads dozens of these.
	if _, ok := keys["CLAUDE_CONFIG_DIR"]; !ok || len(keys) < 20 {
		t.Fatalf("scan found %d keys and no CLAUDE_CONFIG_DIR; it did not read the SDK", len(keys))
	}
	cleared, refused := partitionPrefixes(documentedPluginEnvAllowPrefixes, keys)
	for prefix, key := range refused {
		t.Errorf("documented allowed prefix %s covers %s, which the pinned SDK reads", prefix, key)
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	t.Logf("scanned %d CLAUDE_* keys; cleared prefixes %v", len(keys), cleared)
	t.Logf("keys: %s", strings.Join(sorted, " "))
}
