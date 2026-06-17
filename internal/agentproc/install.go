package agentproc

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// Pinned SDK version. Bump on Triage Factory release after verifying the
// new release in a spike — see scripts/clean-slate.sh notes. Keep the
// package.json template in sync with this constant.
const sdkVersion = "0.2.137"

// Embedded shim that translates the flag-based argv BuildArgs emits into
// Agent SDK Options. Materialized to disk at first install so the Node
// process can `import` from `node_modules/` next to it.
//
//go:embed wrapper.mjs
var wrapperJS []byte

// Embedded lockfile pinning every transitive dependency of the SDK.
// Materialized alongside package.json on every install so `npm ci`
// produces a byte-identical node_modules tree across users running
// the same Triage Factory binary version. Refresh in lockstep with
// sdkVersion bumps: bump the constant, run a fresh install, copy the
// resulting ~/.triagefactory/sdk/package-lock.json over this file.
//
//go:embed package-lock.json
var packageLockJSON []byte

var (
	installOnce sync.Once
	installPath string // populated on success
	installErr  error  // populated on first-run failure; sticky
)

// EnsureSDK installs the Agent SDK + wrapper into ~/.triagefactory/sdk/
// on first call and returns the absolute path to wrapper.mjs. Subsequent
// calls return the cached path. Concurrency-safe via sync.Once. A failure
// here sticks for the lifetime of the process so we don't retry npm
// install on every agent run when the user is missing Node.
//
// The install is idempotent: re-running against an already-populated dir
// re-writes wrapper.mjs + package.json + package-lock.json (cheap) and
// skips `npm ci` when the pinned SDK version is already in node_modules.
func EnsureSDK() (string, error) {
	installOnce.Do(func() {
		installPath, installErr = doInstall()
	})
	return installPath, installErr
}

func doInstall() (string, error) {
	// Surface a missing $HOME as an error (the pre-paths behavior) rather
	// than letting the error-free SDKDir panic underneath.
	if _, err := paths.StateRootErr(); err != nil {
		return "", fmt.Errorf("resolve sdk dir: %w", err)
	}
	// Host-global install: the SDK toolchain is identical for every
	// tenant, so it lives under the toolchain root (TF_TOOLCHAIN_ROOT,
	// else the state root) with no org segment.
	sdkDir := paths.SDKDir()
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", sdkDir, err)
	}

	if err := writePackageJSON(sdkDir); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(sdkDir, "package-lock.json"), packageLockJSON, 0o644); err != nil {
		return "", fmt.Errorf("write package-lock.json: %w", err)
	}

	wrapperPath := filepath.Join(sdkDir, "wrapper.mjs")
	if err := os.WriteFile(wrapperPath, wrapperJS, 0o644); err != nil {
		return "", fmt.Errorf("write wrapper.mjs: %w", err)
	}

	// Check the SDK presence first, then gate node/npm on actually
	// needing to install. The Docker image pre-installs the SDK at
	// build time (multi-stage builder) into ~/.triagefactory/sdk so
	// the runtime alpine layer doesn't need node/npm at all — but
	// the unconditional checkNode/checkNpm pair would error there
	// because nothing on PATH satisfies them. Local-mode users on a
	// fresh laptop still hit the install path and still get the
	// human-readable "install Node 18+" error in that case.
	if sdkAlreadyInstalled(sdkDir) {
		return wrapperPath, nil
	}

	if err := checkNode(); err != nil {
		return "", err
	}
	if err := checkNpm(); err != nil {
		return "", err
	}

	if err := installSDKIfNeeded(sdkDir); err != nil {
		return "", err
	}

	return wrapperPath, nil
}

// sdkAlreadyInstalled returns true when the pinned SDK version is
// present in node_modules. Mirrors the version-pinned check inside
// installSDKIfNeeded so a sdkVersion bump in a future release still
// triggers a re-install (with node/npm available locally) instead of
// silently keeping the stale tree.
func sdkAlreadyInstalled(sdkDir string) bool {
	pkgFile := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json")
	installed, err := readInstalledSDKVersion(pkgFile)
	return err == nil && installed == sdkVersion
}

// writePackageJSON pins the SDK version. We re-write every install pass
// so a Triage Factory upgrade that bumps sdkVersion picks up the new
// pin even if the user already has an older copy installed (the
// installSDKIfNeeded check below will then trigger a re-install).
func writePackageJSON(sdkDir string) error {
	body := fmt.Sprintf(`{
  "name": "triagefactory-sdk-runtime",
  "private": true,
  "type": "module",
  "dependencies": {
    "@anthropic-ai/claude-agent-sdk": "%s"
  }
}
`, sdkVersion)
	return os.WriteFile(filepath.Join(sdkDir, "package.json"), []byte(body), 0o644)
}

// checkNode verifies a usable Node is on PATH. Required floor is 18 —
// the SDK itself documents Node 18+ as the minimum. We surface a
// human-readable error here so a missing-Node user sees "install Node
// 18+" rather than the opaque ENOENT exec.Command would otherwise raise
// when run.go later tries to spawn the wrapper.
func checkNode() error {
	out, err := exec.Command("node", "--version").Output()
	if err != nil {
		return fmt.Errorf("node not found on PATH: install Node 18+ (https://nodejs.org/) — required for the Triage Factory agent runtime")
	}
	major, err := parseNodeMajor(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("parse node --version output %q: %w", string(out), err)
	}
	if major < 18 {
		return fmt.Errorf("node %s is too old; Triage Factory requires Node 18+", strings.TrimSpace(string(out)))
	}
	return nil
}

// checkNpm verifies npm is on PATH. The official Node installer ships
// npm bundled, but distro packages (debian's nodejs without npm) and
// some nvm setups can have node alone. Surface this at first-run with
// a clear message instead of letting it fall out of `npm ci` later as
// an opaque "executable not found" inside the install error.
func checkNpm() error {
	if _, err := exec.Command("npm", "--version").Output(); err != nil {
		return fmt.Errorf("npm not found on PATH: install npm (typically bundled with Node.js — https://nodejs.org/) — required for the Triage Factory agent runtime")
	}
	return nil
}

func parseNodeMajor(version string) (int, error) {
	v := strings.TrimPrefix(version, "v")
	dot := strings.IndexByte(v, '.')
	if dot <= 0 {
		return 0, fmt.Errorf("unexpected format")
	}
	return strconv.Atoi(v[:dot])
}

// installSDKIfNeeded skips when the pinned SDK is already on disk.
// We parse the installed package's version field rather than just
// checking directory existence so a sdkVersion bump in a future release
// re-triggers `npm ci`. JSON parse (not substring match) so npm
// reformatting the file — different whitespace, key order, etc. — never
// causes us to spuriously re-run `npm ci` on every process start.
//
// Uses `npm ci` (not `npm install`) so the embedded package-lock.json is
// authoritative: every Triage Factory binary version produces the same
// transitive dependency tree, and `npm ci` refuses to drift if the
// lockfile is out of sync with package.json — bugs in user environments
// won't be papered over by silent dependency upgrades.
func installSDKIfNeeded(sdkDir string) error {
	pkgFile := filepath.Join(sdkDir, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json")
	if installed, err := readInstalledSDKVersion(pkgFile); err == nil && installed == sdkVersion {
		return nil
	}

	cmd := exec.Command("npm", "ci", "--no-audit", "--no-fund", "--silent")
	cmd.Dir = sdkDir
	cmd.Env = os.Environ()
	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("npm ci in %s failed: %w\n%s", sdkDir, err, string(combined))
	}
	return nil
}

// readInstalledSDKVersion parses node_modules/<sdk>/package.json and
// returns the installed version. Errors (file missing, malformed JSON,
// missing version field) all degrade to an empty string + error so the
// caller treats them as "not installed" and runs `npm ci`.
func readInstalledSDKVersion(pkgFile string) (string, error) {
	body, err := os.ReadFile(pkgFile)
	if err != nil {
		return "", err
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &pkg); err != nil {
		return "", fmt.Errorf("parse %s: %w", pkgFile, err)
	}
	return pkg.Version, nil
}
