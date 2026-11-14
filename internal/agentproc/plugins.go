package agentproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A team's local plugins (domain.TeamSettings.LocalPlugins) reach a run in
// three steps, and this file holds the rules all three share:
//
//   - the settings write validates the list (ValidateLocalPlugins), so a bad
//     entry is refused with its field named;
//   - the spawn prepares it (PreparePlugins): validates it again, resolves
//     each path, refuses one inside a tree the run's agent can write, creates
//     each entry's run state directory and expands {run_state_dir};
//   - the command builder merges each entry's env under TF's own
//     (pluginEnvForSpawn), and the wrapper refuses every tool call until a
//     required plugin is reported loaded (wrapper.mjs, the plugin latch).
//
// Validating twice is deliberate. A path that existed when it was saved can
// be gone by the next run, and the set of allowed env keys depends on the
// operator's TF_PLUGIN_ENV_ALLOW_PREFIXES and on the installed SDK, neither
// of which the settings write can see.

// MaxLocalPlugins caps a team's plugin list.
const MaxLocalPlugins = 8

// RunStateDirPlaceholder is the one placeholder a plugin env value may carry.
// It expands to the entry's directory under the run root, ending in a path
// separator, so "{run_state_dir}sessions" names a child of it.
const RunStateDirPlaceholder = "{run_state_dir}"

// Grant tool names a plugin entry may carry (domain.LocalPlugin.GrantTools).
const (
	PluginGrantAgent = "Agent"
	PluginGrantSkill = "Skill"
)

// maxPluginEnvValueBytes caps one env value.
const maxPluginEnvValueBytes = 1024

// pluginManifest is the file that makes a directory a Claude Code plugin.
var pluginManifest = filepath.Join(".claude-plugin", "plugin.json")

// pluginRunStateRel is where each entry's run state directory lives under
// the run root: <runRoot>/_tfac/plugins/<index>.
var pluginRunStateRel = filepath.Join("_tfac", "plugins")

var (
	pluginEnvKeyRE      = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	pluginPlaceholderRE = regexp.MustCompile(`\{[A-Za-z0-9_]*\}`)
)

// PluginFault is one refused part of a plugin list. Field names the part in
// the settings PATCH body's terms (local_plugins[2].env.FOO), so the handler
// reports it without translating.
type PluginFault struct {
	Field   string
	Message string
}

func (f PluginFault) Error() string { return f.Field + ": " + f.Message }

// ValidateLocalPlugins checks a plugin list the way the settings write
// accepts it, and reports every fault rather than the first. A nil result is
// a list the write may store.
//
// A path is checked against the filesystem of the host running TF, which in
// local mode is the host the runs spawn on.
func ValidateLocalPlugins(list []domain.LocalPlugin) []PluginFault {
	verifyPluginPrefixesIfInstalled()
	var faults []PluginFault
	if len(list) > MaxLocalPlugins {
		faults = append(faults, PluginFault{Field: "local_plugins", Message: fmt.Sprintf("at most %d plugins", MaxLocalPlugins)})
	}
	seen := map[string]int{}
	for i, entry := range list {
		field := fmt.Sprintf("local_plugins[%d]", i)
		resolved, err := resolvePluginDir(entry.Path)
		if err != nil {
			faults = append(faults, PluginFault{Field: field + ".path", Message: err.Error()})
		} else if prev, dup := seen[resolved]; dup {
			faults = append(faults, PluginFault{Field: field + ".path", Message: fmt.Sprintf("names the same directory as local_plugins[%d]", prev)})
		} else {
			seen[resolved] = i
		}
		faults = append(faults, validateGrantTools(field, entry.GrantTools)...)
		for _, key := range sortedKeys(entry.Env) {
			faults = append(faults, validatePluginEnvEntry(field, key, entry.Env[key])...)
		}
	}
	return faults
}

// resolvePluginDir returns the path a plugin entry names, cleaned and with
// every symlink resolved, after checking that it is an absolute path to a
// directory holding .claude-plugin/plugin.json. The resolved path is what is
// passed to Claude Code, bound into the sandbox and matched against what
// Claude Code reports loaded, so all three name one directory.
func resolvePluginDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("is required")
	}
	if strings.ContainsRune(path, 0) {
		return "", errors.New("contains a NUL byte")
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", errors.New("must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("cannot be read: %v", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("cannot be read: %v", err)
	}
	if !info.IsDir() {
		return "", errors.New("is not a directory")
	}
	manifest, err := os.Stat(filepath.Join(resolved, pluginManifest))
	if err != nil || manifest.IsDir() {
		return "", fmt.Errorf("has no %s, so it is not a Claude Code plugin", filepath.ToSlash(pluginManifest))
	}
	return resolved, nil
}

func validateGrantTools(field string, tools []string) []PluginFault {
	var faults []PluginFault
	seen := map[string]bool{}
	for j, tool := range tools {
		f := fmt.Sprintf("%s.grant_tools[%d]", field, j)
		switch {
		case tool == PluginGrantAgent || tool == PluginGrantSkill:
			if seen[tool] {
				faults = append(faults, PluginFault{Field: f, Message: fmt.Sprintf("%s is listed twice", tool)})
			}
			seen[tool] = true
		case strings.HasPrefix(tool, PluginGrantSkill+"("):
			faults = append(faults, PluginFault{Field: f, Message: "per-skill grants are not supported; use Skill"})
		default:
			faults = append(faults, PluginFault{Field: f, Message: fmt.Sprintf("must be %q or %q", PluginGrantAgent, PluginGrantSkill)})
		}
	}
	return faults
}

func validatePluginEnvEntry(field, key, value string) []PluginFault {
	f := field + ".env." + key
	var faults []PluginFault
	if ok, reason := PluginEnvKeyAllowed(key); !ok {
		faults = append(faults, PluginFault{Field: f, Message: reason})
	}
	if err := checkPluginEnvValue(value); err != nil {
		faults = append(faults, PluginFault{Field: f, Message: err.Error()})
	}
	return faults
}

func checkPluginEnvValue(value string) error {
	if len(value) > maxPluginEnvValueBytes {
		return fmt.Errorf("value is longer than %d bytes", maxPluginEnvValueBytes)
	}
	if strings.ContainsRune(value, 0) {
		return errors.New("value contains a NUL byte")
	}
	for _, ph := range pluginPlaceholderRE.FindAllString(value, -1) {
		if ph != RunStateDirPlaceholder {
			return fmt.Errorf("value carries the placeholder %s; the only placeholder is %s", ph, RunStateDirPlaceholder)
		}
	}
	return nil
}

// Plugin is one prepared entry of a run's plugin list: what the spawn passes
// to Claude Code, binds into the sandbox and sets in the run's environment.
type Plugin struct {
	// Path is the resolved plugin directory.
	Path string
	// Required makes the wrapper refuse every tool call until the plugin is
	// reported loaded, and fail the run when it is not.
	Required bool
	// Env is the entry's env as KEY=value, {run_state_dir} expanded, in key
	// order.
	Env []string
	// GrantTools are merged into the run's allowlist.
	GrantTools []string
	// StateDir is the entry's run state directory, which {run_state_dir}
	// expands to.
	StateDir string
	// StateDirCreated reports that StateDir did not exist before this spawn.
	StateDirCreated bool
}

// PluginPrepareContext is what PreparePlugins checks a list against: the
// directories a plugin may not live inside, because the run's agent can write
// to them and so could rewrite the plugin that guards it.
type PluginPrepareContext struct {
	// RunRoot is the run's own tree, which also holds the state directories.
	RunRoot string
	// AddDirs are the run's --add-dir grants.
	AddDirs []string
	// RunsBase is the directory every run root sits under.
	RunsBase string
}

// PluginWarning is one thing PreparePlugins left out of a run rather than
// fail it: an unusable non-required entry, or an env key that is no longer
// allowed. Field uses ValidateLocalPlugins' names.
type PluginWarning struct {
	Field   string
	Message string
	// EnvKeyDropped is set when the warning is a dropped env key.
	EnvKeyDropped bool
}

// ErrPluginNotLoaded marks a run that failed because a required plugin did
// not load: refused at the spawn (PreparePlugins), or reported not loaded by
// the wrapper (LiveRun.Err). Its text is the failure's machine prefix.
var ErrPluginNotLoaded = errors.New("plugin_not_loaded")

// PreparePlugins turns a team's plugin list into what one spawn uses. It runs
// the settings write's checks again, because the filesystem and the allowed
// env keys may have changed since the list was saved. A required entry that
// fails a check fails the spawn (an error wrapping ErrPluginNotLoaded); a
// non-required one is left out with a warning. An env key that is no longer
// allowed is dropped with a warning; its entry still loads.
//
// Each entry's state directory is created at mode 0700. A spawn resuming on a
// run root that was removed recreates it empty, and StateDirCreated says so.
func PreparePlugins(list []domain.LocalPlugin, pc PluginPrepareContext) ([]Plugin, []PluginWarning, error) {
	if len(list) == 0 {
		return nil, nil, nil
	}
	if len(list) > MaxLocalPlugins {
		return nil, nil, fmt.Errorf("%w: the team lists %d plugins, more than %d", ErrPluginNotLoaded, len(list), MaxLocalPlugins)
	}
	verifyPluginPrefixesForSpawn()
	if pc.RunRoot == "" || !filepath.IsAbs(pc.RunRoot) {
		return nil, nil, fmt.Errorf("%w: the run root %q is not an absolute path", ErrPluginNotLoaded, pc.RunRoot)
	}
	forbidden := append([]string{pc.RunRoot, pc.RunsBase}, pc.AddDirs...)

	var (
		out      []Plugin
		warnings []PluginWarning
		seen     = map[string]int{}
	)
	// refuse fails the spawn for a required entry and skips a non-required
	// one with a warning.
	refuse := func(entry domain.LocalPlugin, field, msg string) error {
		if entry.Required {
			return fmt.Errorf("%w: %s: %s", ErrPluginNotLoaded, entry.Path, msg)
		}
		warnings = append(warnings, PluginWarning{Field: field, Message: msg})
		return nil
	}
	for i, entry := range list {
		field := fmt.Sprintf("local_plugins[%d]", i)
		resolved, err := resolvePluginDir(entry.Path)
		if err != nil {
			if rerr := refuse(entry, field+".path", err.Error()); rerr != nil {
				return nil, nil, rerr
			}
			continue
		}
		if prev, dup := seen[resolved]; dup {
			if rerr := refuse(entry, field+".path", fmt.Sprintf("names the same directory as local_plugins[%d]", prev)); rerr != nil {
				return nil, nil, rerr
			}
			continue
		}
		if base, inside := insideAny(resolved, forbidden); inside {
			if rerr := refuse(entry, field+".path", fmt.Sprintf("is inside %s, which the run's agent can write to", base)); rerr != nil {
				return nil, nil, rerr
			}
			continue
		}
		if faults := validateGrantTools(field, entry.GrantTools); len(faults) > 0 {
			if rerr := refuse(entry, faults[0].Field, faults[0].Message); rerr != nil {
				return nil, nil, rerr
			}
			continue
		}
		seen[resolved] = i

		stateRel := filepath.Join(pluginRunStateRel, strconv.Itoa(i))
		stateDir := filepath.Join(pc.RunRoot, stateRel)
		// The run root is the agent's to write, so an earlier engagement on
		// the task can have put a symlink where the state directory goes.
		// Following it would chmod, and point the plugin's state at, whatever
		// it names.
		if err := refuseSymlinkPath(pc.RunRoot, stateRel); err != nil {
			if rerr := refuse(entry, field+".path", err.Error()); rerr != nil {
				return nil, nil, rerr
			}
			continue
		}
		_, statErr := os.Lstat(stateDir)
		created := errors.Is(statErr, os.ErrNotExist)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("%w: create the run state directory for %s: %v", ErrPluginNotLoaded, resolved, err)
		}
		// MkdirAll leaves an existing directory's mode alone, and the umask
		// can narrow a new one but never widen it; chmod makes 0700 hold
		// either way.
		if err := os.Chmod(stateDir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("%w: restrict the run state directory for %s: %v", ErrPluginNotLoaded, resolved, err)
		}

		p := Plugin{
			Path:            resolved,
			Required:        entry.Required,
			GrantTools:      slices.Clone(entry.GrantTools),
			StateDir:        stateDir,
			StateDirCreated: created,
		}
		expansion := stateDir + string(filepath.Separator)
		for _, key := range sortedKeys(entry.Env) {
			value := entry.Env[key]
			envField := field + ".env." + key
			if ok, reason := PluginEnvKeyAllowed(key); !ok {
				warnings = append(warnings, PluginWarning{Field: envField, Message: reason, EnvKeyDropped: true})
				continue
			}
			if err := checkPluginEnvValue(value); err != nil {
				warnings = append(warnings, PluginWarning{Field: envField, Message: err.Error(), EnvKeyDropped: true})
				continue
			}
			p.Env = append(p.Env, key+"="+strings.ReplaceAll(value, RunStateDirPlaceholder, expansion))
		}
		out = append(out, p)
	}
	return out, warnings, nil
}

// insideAny reports the first of bases that path is, or is inside. Bases are
// resolved through symlinks when they exist, the same way path was.
func insideAny(path string, bases []string) (string, bool) {
	for _, base := range bases {
		if strings.TrimSpace(base) == "" {
			continue
		}
		b := filepath.Clean(base)
		if r, err := filepath.EvalSymlinks(b); err == nil {
			b = r
		}
		rel, err := filepath.Rel(b, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return base, true
		}
	}
	return "", false
}

// pluginEnvForSpawn returns the plugins' env entries the spawn may set,
// leaving out every key TF itself sets for this run. TF's set depends on the
// run (its git config block, its engine tuning, its ExtraEnv), so the
// settings write cannot see all of it; a collision is dropped here and
// reported through drop, and TF's value is the one the run gets.
func pluginEnvForSpawn(plugins []Plugin, tfSet []string, drop func(key, reason string)) []string {
	if len(plugins) == 0 {
		return nil
	}
	taken := make(map[string]struct{}, len(tfSet))
	for _, e := range tfSet {
		if eq := strings.IndexByte(e, '='); eq > 0 {
			taken[e[:eq]] = struct{}{}
		}
	}
	var out []string
	for _, p := range plugins {
		for _, e := range p.Env {
			eq := strings.IndexByte(e, '=')
			if eq <= 0 {
				continue
			}
			key := e[:eq]
			if _, clash := taken[key]; clash {
				if drop != nil {
					drop(key, "Triage Factory sets this key for the run")
				}
				continue
			}
			// The settings write and PreparePlugins already refused a
			// disallowed key; checked once more because this is the last
			// point before the key reaches a process.
			if ok, reason := PluginEnvKeyAllowed(key); !ok {
				if drop != nil {
					drop(key, reason)
				}
				continue
			}
			out = append(out, e)
		}
	}
	return out
}

// envPluginInitTimeout overrides how long a run with a required plugin waits
// for Claude Code's init after its first input, in whole seconds.
const envPluginInitTimeout = "TF_PLUGIN_INIT_TIMEOUT"

// defaultPluginInitTimeout is three times the SessionStart cost a guard
// plugin was measured at (about 19 seconds).
const defaultPluginInitTimeout = 60 * time.Second

// PluginInitTimeout resolves TF_PLUGIN_INIT_TIMEOUT. A set value that is not a
// positive whole number of seconds is an error, so a typo fails the spawn
// rather than running on a deadline nobody chose.
func PluginInitTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(envPluginInitTimeout))
	if raw == "" {
		return defaultPluginInitTimeout, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive whole number of seconds", envPluginInitTimeout, raw)
	}
	return time.Duration(n) * time.Second, nil
}

// PluginPaths returns the resolved paths of plugins, in order.
func PluginPaths(plugins []Plugin) []string {
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, p.Path)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PluginsReport is one plugins_loaded report from the wrapper, with how long
// after the spawn it arrived. That is the plugins' SessionStart cost plus the
// wait for the first input.
type PluginsReport struct {
	domain.PluginsLoaded
	SinceSpawn time.Duration
}

// refuseSymlinkPath checks each component of rel under root that exists, and
// refuses one that is a symlink or not a directory. A missing component is
// fine: MkdirAll creates it as a real directory.
func refuseSymlinkPath(root, rel string) error {
	cur := root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("its run state directory %s cannot be checked: %v", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("its run state directory path %s is a symlink", cur)
		}
		if !info.IsDir() {
			return fmt.Errorf("its run state directory path %s is not a directory", cur)
		}
	}
	return nil
}
