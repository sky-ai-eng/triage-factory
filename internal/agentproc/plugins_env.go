package agentproc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// A plugin env key reaches a run's environment beside the credentials and
// configuration TF resolved for it, so the keys a team may set are narrow.
// PluginEnvKeyAllowed is the one rule, called by the settings write and again
// at spawn.
//
// Every CLAUDE_* key is refused by default. Claude Code reads many of them
// (CLAUDE_SECURESTORAGE_CONFIG_DIR relocates its credential store,
// CLAUDE_AI_ORIGIN and CLAUDE_BRIDGE_* redirect its endpoints) and the set
// grows with each SDK release, so no list of the dangerous ones stays
// complete. Some plugins name their own keys CLAUDE_<PLUGIN>_*; the operator,
// not a team admin, may allow such a prefix with TF_PLUGIN_ENV_ALLOW_PREFIXES.
// An allowed prefix is honored only once the installed SDK has been scanned
// and found to read no key under it (VerifyPluginEnvAllowPrefixes).

// envPluginEnvAllowPrefixes is the operator's comma list of CLAUDE_ prefixes
// a plugin env key may use.
const envPluginEnvAllowPrefixes = "TF_PLUGIN_ENV_ALLOW_PREFIXES"

// pluginEnvRefusedPrefixes are refused key families, by prefix.
var pluginEnvRefusedPrefixes = []string{
	// TF and model
	"TRIAGE_FACTORY_", "TF_", "ANTHROPIC_", "BUN_", "CLAUDE_",
	// Git and forge
	"GH_", "GITHUB_", "GIT_", "SSH_",
	// Loaders, interpreters and shells. A plugin's env reaches the whole run
	// process, the agent's own Bash tool included, not just the plugin's hooks.
	"LD_", "DYLD_", "PYTHON", "PERL", "RUBY", "NODE_", "JAVA_", "_JAVA_", "LUA_", "GLIBC_",
	// Package managers, whose config redirects where a command fetches from
	"NPM_CONFIG_", "PIP_", "UV_", "CARGO_",
	// Network and trust
	"SSL_", "CURL_",
	// Identity and paths
	"XDG_", "AWS_", "AZURE_", "GOOGLE_",
}

// pluginEnvRefusedKeys are refused keys, exactly.
var pluginEnvRefusedKeys = map[string]struct{}{
	"CLAUDECODE":          {},
	"NODE_OPTIONS":        {},
	"NODE_PATH":           {},
	"NODE_EXTRA_CA_CERTS": {},
	"BASH_ENV":            {},
	"ENV":                 {},
	"SHELL":               {},
	"ZDOTDIR":             {},
	"GOPROXY":             {},
	"GOFLAGS":             {},
	"GOPRIVATE":           {},
	"GONOSUMDB":           {},
	"GONOSUMCHECK":        {},
	"GOSUMDB":             {},
	"GOINSECURE":          {},
	"GOTOOLCHAIN":         {},
	"SSLKEYLOGFILE":       {},
	"SHELLOPTS":           {},
	"PS4":                 {},
	"IFS":                 {},
	"HTTP_PROXY":          {},
	"HTTPS_PROXY":         {},
	"ALL_PROXY":           {},
	"NO_PROXY":            {},
	"REQUESTS_CA_BUNDLE":  {},
	"HOME":                {},
	"USERPROFILE":         {},
	"APPDATA":             {},
	"LOCALAPPDATA":        {},
	"PATH":                {},
	"PATHEXT":             {},
	"COMSPEC":             {},
	"TMPDIR":              {},
	"TEMP":                {},
	"TMP":                 {},
}

// pluginEnvRefusedSubstrings refuse any key that looks like it carries a
// credential.
var pluginEnvRefusedSubstrings = []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "_KEY", "AUTH"}

// PluginEnvKeyAllowed reports whether a team's plugin entry may set key in a
// run's environment, and why not when it may not.
func PluginEnvKeyAllowed(key string) (bool, string) {
	if !pluginEnvKeyRE.MatchString(key) {
		return false, "key must match ^[A-Z][A-Z0-9_]{0,63}$"
	}
	for _, sub := range pluginEnvRefusedSubstrings {
		if strings.Contains(key, sub) {
			return false, fmt.Sprintf("key contains %s, so it may carry a credential", sub)
		}
	}
	// The proxy keys are read in either case; the key pattern already
	// refuses lowercase, and this keeps the check honest if it ever widens.
	if _, refused := pluginEnvRefusedKeys[strings.ToUpper(key)]; refused {
		return false, "key is reserved for Triage Factory, the agent runtime or the host"
	}
	if strings.HasPrefix(key, "CLAUDE_") {
		if allowedClaudePrefixFor(key) {
			return true, ""
		}
		if pending := configuredPrefixFor(key); pending != "" {
			return false, fmt.Sprintf("the operator allows the prefix %s, but it has not been checked against the installed agent SDK yet, or the SDK reads a key under it", pending)
		}
		return false, "CLAUDE_ keys are reserved for Claude Code; the operator can allow a plugin's own prefix with " + envPluginEnvAllowPrefixes
	}
	for _, p := range pluginEnvRefusedPrefixes {
		if strings.HasPrefix(key, p) {
			return false, fmt.Sprintf("keys starting %s are reserved for Triage Factory, the agent runtime or the host", p)
		}
	}
	// Every key the credential resolver strips from a run's inherited env
	// decides which provider and account the run's model calls go to, so a
	// plugin may not set one either (CLOUD_ML_REGION, VERTEX_REGION_*, ...).
	if isCredentialEnvKey(key) {
		return false, "key selects the run's model provider or credentials"
	}
	return true, ""
}

// pluginPrefixGuard holds the allowed prefixes once verified. Verification
// is per configured value: a changed TF_PLUGIN_ENV_ALLOW_PREFIXES is
// unverified until scanned again.
var pluginPrefixGuard struct {
	mu         sync.Mutex
	configured string   // the raw value the verified list was computed for
	verified   []string // configured prefixes the SDK scan cleared
	done       bool
}

// ParsePluginEnvAllowPrefixes parses an allow-prefix list. Each prefix must
// start with CLAUDE_, end in _, and have at least two segments after
// CLAUDE_, so no prefix can cover a whole family of Claude Code's own keys.
func ParsePluginEnvAllowPrefixes(raw string) ([]string, error) {
	var out []string
	var errs []error
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if err := checkAllowPrefix(p); err != nil {
			errs = append(errs, err)
			continue
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, errors.Join(errs...)
}

var allowPrefixSegmentRE = regexp.MustCompile(`^[A-Z0-9]+$`)

func checkAllowPrefix(p string) error {
	if !strings.HasPrefix(p, "CLAUDE_") || !strings.HasSuffix(p, "_") {
		return fmt.Errorf("%s: %q must start with CLAUDE_ and end in _", envPluginEnvAllowPrefixes, p)
	}
	segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(p, "CLAUDE_"), "_"), "_")
	if len(segs) < 2 {
		return fmt.Errorf("%s: %q needs at least two segments after CLAUDE_ (CLAUDE_<PLUGIN>_<PART>_)", envPluginEnvAllowPrefixes, p)
	}
	for _, s := range segs {
		if !allowPrefixSegmentRE.MatchString(s) {
			return fmt.Errorf("%s: %q has an empty or invalid segment", envPluginEnvAllowPrefixes, p)
		}
	}
	return nil
}

// configuredAllowPrefixes reads the operator's prefixes, leaving out any
// malformed one (VerifyPluginEnvAllowPrefixes reports those).
func configuredAllowPrefixes() (string, []string) {
	raw := os.Getenv(envPluginEnvAllowPrefixes)
	prefixes, _ := ParsePluginEnvAllowPrefixes(raw)
	return raw, prefixes
}

func allowedClaudePrefixFor(key string) bool {
	raw, _ := configuredAllowPrefixes()
	pluginPrefixGuard.mu.Lock()
	defer pluginPrefixGuard.mu.Unlock()
	if !pluginPrefixGuard.done || pluginPrefixGuard.configured != raw {
		return false
	}
	for _, p := range pluginPrefixGuard.verified {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func configuredPrefixFor(key string) string {
	_, prefixes := configuredAllowPrefixes()
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return p
		}
	}
	return ""
}

// VerifyPluginEnvAllowPrefixes scans the agent SDK installed at sdkDir for
// every CLAUDE_* key it names and clears each configured prefix that covers
// none of them. A prefix that covers one is refused, and returned in the
// error. A scan that cannot run leaves every prefix refused: "could not scan"
// is never read as "no match".
//
// Called at startup when the SDK is installed, and after every install
// before the first spawn (EnsureSDK's callers). Cheap to call again: a value
// already verified returns at once.
func VerifyPluginEnvAllowPrefixes(sdkDir string) error {
	raw, prefixes := configuredAllowPrefixes()
	_, parseErr := ParsePluginEnvAllowPrefixes(raw)

	pluginPrefixGuard.mu.Lock()
	if pluginPrefixGuard.done && pluginPrefixGuard.configured == raw {
		pluginPrefixGuard.mu.Unlock()
		return parseErr
	}
	pluginPrefixGuard.mu.Unlock()

	if len(prefixes) == 0 {
		setVerifiedPrefixes(raw, nil)
		return parseErr
	}
	keys, err := ScanSDKClaudeKeys(sdkDir)
	if err != nil {
		// Not marked done, so a later call retries the scan.
		return errors.Join(parseErr, fmt.Errorf("%s: no prefix is honored until the agent SDK can be scanned: %w", envPluginEnvAllowPrefixes, err))
	}
	cleared, refused := partitionPrefixes(prefixes, keys)
	setVerifiedPrefixes(raw, cleared)
	var errs []error
	for p, key := range refused {
		errs = append(errs, fmt.Errorf("%s: refusing %q: the installed agent SDK reads %s", envPluginEnvAllowPrefixes, p, key))
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(append([]error{parseErr}, errs...)...)
}

func setVerifiedPrefixes(raw string, cleared []string) {
	pluginPrefixGuard.mu.Lock()
	defer pluginPrefixGuard.mu.Unlock()
	pluginPrefixGuard.configured = raw
	pluginPrefixGuard.verified = cleared
	pluginPrefixGuard.done = true
}

// resetPluginPrefixGuard forgets every verification. Tests only.
func resetPluginPrefixGuard() {
	pluginPrefixGuard.mu.Lock()
	defer pluginPrefixGuard.mu.Unlock()
	pluginPrefixGuard.configured = ""
	pluginPrefixGuard.verified = nil
	pluginPrefixGuard.done = false
}

// partitionPrefixes splits prefixes into those covering none of keys and
// those covering one, with the first key each covers.
func partitionPrefixes(prefixes []string, keys map[string]struct{}) ([]string, map[string]string) {
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var cleared []string
	refused := map[string]string{}
	for _, p := range prefixes {
		hit := ""
		for _, k := range sorted {
			if strings.HasPrefix(k, p) {
				hit = k
				break
			}
		}
		if hit == "" {
			cleared = append(cleared, p)
		} else {
			refused[p] = hit
		}
	}
	return cleared, refused
}

var sdkClaudeKeyRE = regexp.MustCompile(`CLAUDE_[A-Z0-9_]+`)

// ScanSDKClaudeKeys returns every CLAUDE_[A-Z0-9_]+ literal in the agent SDK
// installed at sdkDir: the SDK package's JavaScript and the platform Claude
// Code binary beside it, where most of the reads live. It fails unless it
// found both, so an incomplete install cannot pass as a clean one.
func ScanSDKClaudeKeys(sdkDir string) (map[string]struct{}, error) {
	scope := filepath.Join(sdkDir, "node_modules", "@anthropic-ai")
	entries, err := os.ReadDir(scope)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", scope, err)
	}
	keys := map[string]struct{}{}
	var scripts, binaries int
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || (name != "claude-agent-sdk" && !strings.HasPrefix(name, "claude-agent-sdk-")) {
			continue
		}
		pkgDir := filepath.Join(scope, name)
		werr := filepath.WalkDir(pkgDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" && path != pkgDir {
					return filepath.SkipDir
				}
				return nil
			}
			base := d.Name()
			switch {
			case base == "claude" || base == "claude.exe":
				binaries++
			case strings.HasSuffix(base, ".mjs") || strings.HasSuffix(base, ".js") || strings.HasSuffix(base, ".cjs"):
				scripts++
			default:
				return nil
			}
			return scanFileForClaudeKeys(path, keys)
		})
		if werr != nil {
			return nil, fmt.Errorf("scan %s: %w", pkgDir, werr)
		}
	}
	if scripts == 0 {
		return nil, fmt.Errorf("no agent SDK scripts under %s", scope)
	}
	if binaries == 0 {
		return nil, fmt.Errorf("no Claude Code binary under %s", scope)
	}
	return keys, nil
}

// scanFileForClaudeKeys streams path through the key pattern a chunk at a
// time. A match that ends before the end of the buffer is complete, since the
// byte after it is not a key byte, and is recorded. A match that reaches the
// end may continue into the next chunk, so it is not recorded yet: the buffer
// is kept from its start, and so is the tail that could be the start of a
// "CLAUDE_" split by the boundary, and the next round reads it whole.
func scanFileForClaudeKeys(path string, keys map[string]struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const chunk = 1 << 20
	// partialPrefix is the longest tail that can be the start of a key split
	// by the boundary: "CLAUDE_" whole, with its first key byte in the next
	// chunk.
	partialPrefix := len("CLAUDE_")
	r := bufio.NewReaderSize(f, chunk)
	var buf []byte
	tmp := make([]byte, chunk)
	for {
		n, rerr := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		eof := errors.Is(rerr, io.EOF)
		if rerr != nil && !eof {
			return rerr
		}
		keep := len(buf)
		for _, loc := range sdkClaudeKeyRE.FindAllIndex(buf, -1) {
			if loc[1] == len(buf) && !eof {
				keep = loc[0]
				continue
			}
			keys[string(buf[loc[0]:loc[1]])] = struct{}{}
		}
		if eof {
			return nil
		}
		if tail := len(buf) - partialPrefix; tail < keep {
			keep = max(tail, 0)
		}
		buf = append(buf[:0], buf[keep:]...)
	}
}

// verifyPluginPrefixesForSpawn makes sure the operator's allowed prefixes
// have been checked against an installed SDK before a spawn reads them,
// installing the SDK first if it is not there yet: the scan runs after the
// install completes and before the first spawn. With no prefix configured it
// does nothing. A var so tests do not install an SDK.
var verifyPluginPrefixesForSpawn = func() {
	if _, prefixes := configuredAllowPrefixes(); len(prefixes) == 0 {
		return
	}
	if _, err := EnsureSDK(); err != nil {
		agentprocLog.Error("plugin env allow prefixes stay refused: the agent SDK could not be installed to scan", "error", err)
		return
	}
	if err := VerifyPluginEnvAllowPrefixes(paths.SDKDir()); err != nil {
		agentprocLog.Error("plugin env allow prefixes refused", "error", err)
	}
}

// verifyPluginPrefixesIfInstalled is the settings write's version: it scans an
// SDK that is already installed and never installs one, so a write before the
// first run refuses an allowed-prefix key as not yet checked.
var verifyPluginPrefixesIfInstalled = func() {
	if _, prefixes := configuredAllowPrefixes(); len(prefixes) == 0 {
		return
	}
	dir := paths.SDKDir()
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return
	}
	if err := VerifyPluginEnvAllowPrefixes(dir); err != nil {
		agentprocLog.Error("plugin env allow prefixes refused", "error", err)
	}
}
