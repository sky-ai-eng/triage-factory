package agentproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// isolatePluginEnv gives a test the plugin-prefix state its assertions assume:
// no operator-allowed prefix, no SDK scan or install on a spawn or a settings
// write, and a guard that forgets what an earlier test verified.
func isolatePluginEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envPluginEnvAllowPrefixes, "")
	prevSpawn, prevInstalled := verifyPluginPrefixesForSpawn, verifyPluginPrefixesIfInstalled
	verifyPluginPrefixesForSpawn = func() {}
	verifyPluginPrefixesIfInstalled = func() {}
	resetPluginPrefixGuard()
	t.Cleanup(func() {
		verifyPluginPrefixesForSpawn, verifyPluginPrefixesIfInstalled = prevSpawn, prevInstalled
		resetPluginPrefixGuard()
	})
}

// newPluginDir returns a directory holding .claude-plugin/plugin.json, with
// symlinks in its path already resolved so a test can compare it with what
// resolvePluginDir returns.
func newPluginDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, pluginManifest)
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"name":"fixture"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// requireFault fails unless faults holds one on field whose message contains
// substr.
func requireFault(t *testing.T, faults []PluginFault, field, substr string) {
	t.Helper()
	for _, f := range faults {
		if f.Field == field && strings.Contains(f.Message, substr) {
			return
		}
	}
	t.Errorf("no fault on %s containing %q; got %v", field, substr, faults)
}

func TestValidateLocalPlugins_AcceptsValidList(t *testing.T) {
	isolatePluginEnv(t)
	list := []domain.LocalPlugin{
		{Path: newPluginDir(t)},
		{
			Path:       newPluginDir(t),
			Required:   true,
			Env:        map[string]string{"MY_PLUGIN_MODE": "strict", "STATE_FILE": "{run_state_dir}state.json"},
			GrantTools: []string{PluginGrantAgent, PluginGrantSkill},
		},
	}
	if faults := ValidateLocalPlugins(list); len(faults) != 0 {
		t.Errorf("faults = %v, want none", faults)
	}
	if faults := ValidateLocalPlugins(nil); len(faults) != 0 {
		t.Errorf("faults for a nil list = %v, want none", faults)
	}
}

func TestValidateLocalPlugins_PathRejections(t *testing.T) {
	isolatePluginEnv(t)
	root := t.TempDir()
	aFile := filepath.Join(root, "a-file")
	if err := os.WriteFile(aFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	noManifest := filepath.Join(root, "no-manifest")
	if err := os.MkdirAll(noManifest, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestIsDir := filepath.Join(root, "manifest-is-dir")
	if err := os.MkdirAll(filepath.Join(manifestIsDir, pluginManifest), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"empty", "", "is required"},
		{"blank", "   ", "is required"},
		{"relative", filepath.Join("plugins", "guard"), "must be an absolute path"},
		{"NUL byte", filepath.Join(root, "a\x00b"), "NUL byte"},
		{"missing", filepath.Join(root, "gone"), "cannot be read"},
		{"a file, not a directory", aFile, "is not a directory"},
		{"no plugin.json", noManifest, "has no .claude-plugin/plugin.json"},
		{"plugin.json is a directory", manifestIsDir, "has no .claude-plugin/plugin.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			faults := ValidateLocalPlugins([]domain.LocalPlugin{{Path: c.path}})
			if len(faults) != 1 {
				t.Fatalf("faults = %v, want exactly one", faults)
			}
			requireFault(t, faults, "local_plugins[0].path", c.want)
		})
	}
}

func TestValidateLocalPlugins_NamesTheFailingEntry(t *testing.T) {
	isolatePluginEnv(t)
	good := newPluginDir(t)
	faults := ValidateLocalPlugins([]domain.LocalPlugin{
		{Path: good},
		{Path: good + "-gone"},
		{Path: good, Env: map[string]string{"TF_X": "1"}},
	})
	requireFault(t, faults, "local_plugins[1].path", "cannot be read")
	requireFault(t, faults, "local_plugins[2].path", "names the same directory as local_plugins[0]")
	requireFault(t, faults, "local_plugins[2].env.TF_X", "reserved")
	if len(faults) != 3 {
		t.Errorf("faults = %v, want 3", faults)
	}
}

func TestValidateLocalPlugins_TooManyEntries(t *testing.T) {
	isolatePluginEnv(t)
	var list []domain.LocalPlugin
	for i := 0; i < MaxLocalPlugins+1; i++ {
		list = append(list, domain.LocalPlugin{Path: newPluginDir(t)})
	}
	faults := ValidateLocalPlugins(list)
	if len(faults) != 1 {
		t.Fatalf("faults = %v, want exactly one", faults)
	}
	requireFault(t, faults, "local_plugins", fmt.Sprintf("at most %d plugins", MaxLocalPlugins))

	if faults := ValidateLocalPlugins(list[:MaxLocalPlugins]); len(faults) != 0 {
		t.Errorf("%d entries: faults = %v, want none", MaxLocalPlugins, faults)
	}
}

func TestValidateLocalPlugins_DuplicateThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege on Windows")
	}
	isolatePluginEnv(t)
	dir := newPluginDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	faults := ValidateLocalPlugins([]domain.LocalPlugin{{Path: dir}, {Path: link}})
	if len(faults) != 1 {
		t.Fatalf("faults = %v, want exactly one", faults)
	}
	requireFault(t, faults, "local_plugins[1].path", "names the same directory as local_plugins[0]")
}

func TestValidateLocalPlugins_GrantTools(t *testing.T) {
	isolatePluginEnv(t)
	cases := []struct {
		name  string
		tools []string
		field string
		want  string
	}{
		{"per-skill grant", []string{"Skill(foo)"}, "local_plugins[0].grant_tools[0]", "per-skill grants are not supported"},
		{"unknown tool", []string{"Bash"}, "local_plugins[0].grant_tools[0]", `must be "Agent" or "Skill"`},
		{"lowercase", []string{"agent"}, "local_plugins[0].grant_tools[0]", `must be "Agent" or "Skill"`},
		{"empty name", []string{""}, "local_plugins[0].grant_tools[0]", `must be "Agent" or "Skill"`},
		{"Agent twice", []string{"Agent", "Agent"}, "local_plugins[0].grant_tools[1]", "Agent is listed twice"},
		{"Skill twice", []string{"Skill", "Agent", "Skill"}, "local_plugins[0].grant_tools[2]", "Skill is listed twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			faults := ValidateLocalPlugins([]domain.LocalPlugin{{Path: newPluginDir(t), GrantTools: c.tools}})
			if len(faults) != 1 {
				t.Fatalf("faults = %v, want exactly one", faults)
			}
			requireFault(t, faults, c.field, c.want)
		})
	}
}

func TestValidateLocalPlugins_EnvKeyRejections(t *testing.T) {
	isolatePluginEnv(t)
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"lowercase", "lower", "key must match"},
		{"leading digit", "1ABC", "key must match"},
		{"leading underscore", "_ABC", "key must match"},
		{"dash", "MY-KEY", "key must match"},
		{"empty", "", "key must match"},
		{"65 characters", "A" + strings.Repeat("B", 64), "key must match"},

		{"TF_", "TF_X", "reserved"},
		{"TRIAGE_FACTORY_", "TRIAGE_FACTORY_X", "reserved"},
		{"ANTHROPIC_", "ANTHROPIC_BASE_URL", "reserved"},
		{"BUN_", "BUN_X", "reserved"},
		{"CLAUDECODE", "CLAUDECODE", "reserved"},
		{"CLAUDE_", "CLAUDE_X", "reserved for Claude Code"},
		{"GH_", "GH_X", "reserved"},
		{"GITHUB_", "GITHUB_X", "reserved"},
		{"GIT_", "GIT_DIR", "reserved"},
		{"SSH_", "SSH_X", "reserved"},
		{"SSH_AUTH_SOCK", "SSH_AUTH_SOCK", "AUTH"},
		{"NODE_OPTIONS", "NODE_OPTIONS", "reserved"},
		{"NODE_PATH", "NODE_PATH", "reserved"},
		{"NODE_EXTRA_CA_CERTS", "NODE_EXTRA_CA_CERTS", "reserved"},
		{"LD_", "LD_PRELOAD", "reserved"},
		{"DYLD_", "DYLD_X", "reserved"},
		{"PYTHON", "PYTHONPATH", "reserved"},
		{"PERL5", "PERL5LIB", "reserved"},
		{"RUBY", "RUBYOPT", "reserved"},
		{"BASH_ENV", "BASH_ENV", "reserved"},
		{"ENV", "ENV", "reserved"},
		{"SHELLOPTS", "SHELLOPTS", "reserved"},
		{"PS4", "PS4", "reserved"},
		{"IFS", "IFS", "reserved"},
		{"HTTP_PROXY", "HTTP_PROXY", "reserved"},
		{"HTTPS_PROXY", "HTTPS_PROXY", "reserved"},
		{"ALL_PROXY", "ALL_PROXY", "reserved"},
		{"NO_PROXY", "NO_PROXY", "reserved"},
		{"SSL_", "SSL_CERT_FILE", "reserved"},
		{"CURL_", "CURL_CA_BUNDLE", "reserved"},
		{"REQUESTS_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "reserved"},
		{"HOME", "HOME", "reserved"},
		{"USERPROFILE", "USERPROFILE", "reserved"},
		{"APPDATA", "APPDATA", "reserved"},
		{"LOCALAPPDATA", "LOCALAPPDATA", "reserved"},
		{"PATH", "PATH", "reserved"},
		{"PATHEXT", "PATHEXT", "reserved"},
		{"COMSPEC", "COMSPEC", "reserved"},
		{"TMPDIR", "TMPDIR", "reserved"},
		{"TEMP", "TEMP", "reserved"},
		{"TMP", "TMP", "reserved"},
		{"XDG_", "XDG_CONFIG_HOME", "reserved"},
		{"AWS_", "AWS_X", "reserved"},
		{"AZURE_", "AZURE_X", "reserved"},
		{"GOOGLE_", "GOOGLE_X", "reserved"},

		{"TOKEN", "MY_TOKEN", "TOKEN"},
		{"SECRET", "MY_SECRET", "SECRET"},
		{"PASSWORD", "DB_PASSWORD", "PASSWORD"},
		{"CREDENTIAL", "X_CREDENTIAL", "CREDENTIAL"},
		{"_KEY", "API_KEY", "_KEY"},
		{"AUTH", "MYAUTH", "AUTH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			faults := ValidateLocalPlugins([]domain.LocalPlugin{{
				Path: newPluginDir(t),
				Env:  map[string]string{c.key: "1"},
			}})
			if len(faults) != 1 {
				t.Fatalf("env key %q: faults = %v, want exactly one", c.key, faults)
			}
			requireFault(t, faults, "local_plugins[0].env."+c.key, c.want)
			if ok, _ := PluginEnvKeyAllowed(c.key); ok {
				t.Errorf("PluginEnvKeyAllowed(%q) = true", c.key)
			}
		})
	}
}

// Names that share a letter run with a refused one but are not it. The rule
// matches whole keys for the exact entries and prefixes only where the
// family ends in an underscore or is spelled as a bare stem.
func TestPluginEnvKeyAllowed_AcceptsOrdinaryKeys(t *testing.T) {
	isolatePluginEnv(t)
	for _, key := range []string{"MY_PLUGIN_MODE", "LOG_LEVEL", "TEMPLATE_DIR", "ENVIRONMENT", "A", "X1_Y2"} {
		if ok, reason := PluginEnvKeyAllowed(key); !ok {
			t.Errorf("PluginEnvKeyAllowed(%q) refused: %s", key, reason)
		}
	}
}

func TestValidateLocalPlugins_EnvValueRejections(t *testing.T) {
	isolatePluginEnv(t)
	cases := []struct {
		name  string
		value string
		want  string // empty means accepted
	}{
		{"exactly 1024 bytes", strings.Repeat("a", 1024), ""},
		{"1025 bytes", strings.Repeat("a", 1025), "longer than 1024 bytes"},
		{"NUL byte", "a\x00b", "NUL byte"},
		{"run state dir placeholder", "{run_state_dir}sessions", ""},
		{"placeholder twice", "{run_state_dir}a:{run_state_dir}b", ""},
		{"other placeholder", "{home}/x", "{home}"},
		{"empty braces", "x{}y", "{}"},
		{"placeholder beside a valid one", "{run_state_dir}{other}", "{other}"},
		{"braces holding punctuation are literal", `{"a":1}`, ""},
		{"empty value", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			faults := ValidateLocalPlugins([]domain.LocalPlugin{{
				Path: newPluginDir(t),
				Env:  map[string]string{"MY_VALUE": c.value},
			}})
			if c.want == "" {
				if len(faults) != 0 {
					t.Fatalf("faults = %v, want none", faults)
				}
				return
			}
			if len(faults) != 1 {
				t.Fatalf("faults = %v, want exactly one", faults)
			}
			requireFault(t, faults, "local_plugins[0].env.MY_VALUE", c.want)
		})
	}

	t.Run("a placeholder names the one allowed", func(t *testing.T) {
		faults := ValidateLocalPlugins([]domain.LocalPlugin{{Path: newPluginDir(t), Env: map[string]string{"MY_VALUE": "{other}"}}})
		requireFault(t, faults, "local_plugins[0].env.MY_VALUE", RunStateDirPlaceholder)
	})
}

func TestValidateLocalPlugins_ReportsEveryFault(t *testing.T) {
	isolatePluginEnv(t)
	good := newPluginDir(t)
	faults := ValidateLocalPlugins([]domain.LocalPlugin{
		{
			Path:       "relative/path",
			GrantTools: []string{"Bash", "Skill(x)", "Agent", "Agent"},
			Env:        map[string]string{"TF_X": "1", "lower": "1", "OK_NAME": "fine", "BAD_VALUE": "{nope}"},
		},
		{
			Path: good + "-gone",
			Env:  map[string]string{"NUL_VALUE": "a\x00"},
		},
		{Path: good},
		{Path: good},
	})
	want := []struct{ field, msg string }{
		{"local_plugins[0].path", "absolute"},
		{"local_plugins[0].grant_tools[0]", "must be"},
		{"local_plugins[0].grant_tools[1]", "per-skill"},
		{"local_plugins[0].grant_tools[3]", "listed twice"},
		{"local_plugins[0].env.TF_X", "reserved"},
		{"local_plugins[0].env.lower", "key must match"},
		{"local_plugins[0].env.BAD_VALUE", "{nope}"},
		{"local_plugins[1].path", "cannot be read"},
		{"local_plugins[1].env.NUL_VALUE", "NUL byte"},
		{"local_plugins[3].path", "names the same directory as local_plugins[2]"},
	}
	for _, w := range want {
		requireFault(t, faults, w.field, w.msg)
	}
	if len(faults) != len(want) {
		t.Errorf("got %d faults, want %d: %v", len(faults), len(want), faults)
	}
}

func TestPluginFault_Error(t *testing.T) {
	f := PluginFault{Field: "local_plugins[2].env.FOO", Message: "nope"}
	if got, want := f.Error(), "local_plugins[2].env.FOO: nope"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestResolvePluginDir_ResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege on Windows")
	}
	dir := newPluginDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePluginDir(link + string(filepath.Separator) + ".")
	if err != nil {
		t.Fatalf("resolvePluginDir: %v", err)
	}
	if got != dir {
		t.Errorf("resolvePluginDir = %q, want %q", got, dir)
	}
}

func TestPreparePlugins_NilListIsNil(t *testing.T) {
	isolatePluginEnv(t)
	for _, list := range [][]domain.LocalPlugin{nil, {}} {
		plugins, warnings, err := PreparePlugins(list, PluginPrepareContext{})
		if plugins != nil || warnings != nil || err != nil {
			t.Errorf("PreparePlugins(%v) = %v, %v, %v; want nil, nil, nil", list, plugins, warnings, err)
		}
	}
}

// prepareCtx returns a context whose run root, runs base and one add-dir are
// distinct sibling directories, none of them holding a plugin.
func prepareCtx(t *testing.T) PluginPrepareContext {
	t.Helper()
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	return PluginPrepareContext{
		RunRoot:  resolve(t.TempDir()),
		RunsBase: resolve(t.TempDir()),
		AddDirs:  []string{resolve(t.TempDir())},
	}
}

func TestPreparePlugins_CreatesStateDir(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	list := []domain.LocalPlugin{{Path: newPluginDir(t)}}

	first, warnings, err := PreparePlugins(list, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if len(first) != 1 {
		t.Fatalf("got %d plugins, want 1", len(first))
	}
	wantDir := filepath.Join(pc.RunRoot, "_tfac", "plugins", "0")
	if first[0].StateDir != wantDir {
		t.Errorf("StateDir = %q, want %q", first[0].StateDir, wantDir)
	}
	if !first[0].StateDirCreated {
		t.Error("StateDirCreated = false on the first spawn")
	}
	info, err := os.Stat(wantDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("state dir not created: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("state dir mode = %o, want 700", perm)
		}
	}

	second, _, err := PreparePlugins(list, pc)
	if err != nil {
		t.Fatalf("second PreparePlugins: %v", err)
	}
	if second[0].StateDirCreated {
		t.Error("StateDirCreated = true when the directory already existed")
	}
}

func TestPreparePlugins_StateDirModeIsTightenedWhenItExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	existing := filepath.Join(pc.RunRoot, "_tfac", "plugins", "0")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PreparePlugins([]domain.LocalPlugin{{Path: newPluginDir(t)}}, pc); err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("existing state dir mode = %o, want 700", perm)
	}
}

func TestPreparePlugins_StateDirIndexIsTheListIndex(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{
		{Path: filepath.Join(pc.RunRoot, "..", "missing-plugin")},
		{Path: newPluginDir(t)},
	}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if len(warnings) != 1 || len(plugins) != 1 {
		t.Fatalf("plugins = %v, warnings = %v; want the second entry loaded and one warning", plugins, warnings)
	}
	want := filepath.Join(pc.RunRoot, "_tfac", "plugins", "1")
	if plugins[0].StateDir != want {
		t.Errorf("StateDir = %q, want %q (the entry's list index, which {run_state_dir} paths and the positional load tag share)", plugins[0].StateDir, want)
	}
}

func TestPreparePlugins_ExpandsRunStateDirAndSortsEnv(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{{
		Path: newPluginDir(t),
		Env: map[string]string{
			"ZED":      "last",
			"SESSIONS": "{run_state_dir}sessions",
			"ALPHA":    "{run_state_dir}",
			"MID":      "literal={run_state_dir}x:{run_state_dir}y",
		},
	}}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	sep := string(filepath.Separator)
	dir := plugins[0].StateDir + sep
	want := []string{
		"ALPHA=" + dir,
		"MID=literal=" + dir + "x:" + dir + "y",
		"SESSIONS=" + dir + "sessions",
		"ZED=last",
	}
	if !slices.Equal(plugins[0].Env, want) {
		t.Errorf("Env = %v\nwant %v", plugins[0].Env, want)
	}
}

func TestPreparePlugins_CopiesPathRequiredAndGrants(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	grants := []string{PluginGrantAgent, PluginGrantSkill}
	dir := newPluginDir(t)
	plugins, _, err := PreparePlugins([]domain.LocalPlugin{{Path: dir, Required: true, GrantTools: grants}}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	p := plugins[0]
	if p.Path != dir || !p.Required || !slices.Equal(p.GrantTools, grants) {
		t.Errorf("plugin = %+v; want path %q, required, grants %v", p, dir, grants)
	}
	// The list the caller keeps must not alias the prepared one.
	p.GrantTools[0] = "mutated"
	if grants[0] != PluginGrantAgent {
		t.Error("GrantTools aliases the caller's slice")
	}
	if got := PluginPaths(plugins); !slices.Equal(got, []string{dir}) {
		t.Errorf("PluginPaths = %v, want [%s]", got, dir)
	}
	if got := PluginPaths(nil); len(got) != 0 {
		t.Errorf("PluginPaths(nil) = %v, want empty", got)
	}
}

func TestPreparePlugins_ResolvesSymlinkedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege on Windows")
	}
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	dir := newPluginDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	plugins, _, err := PreparePlugins([]domain.LocalPlugin{{Path: link}}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if plugins[0].Path != dir {
		t.Errorf("Path = %q, want the resolved %q", plugins[0].Path, dir)
	}
}

func TestPreparePlugins_RequiredEntryFailsTheSpawn(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	aFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(aFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dup := newPluginDir(t)
	cases := []struct {
		name string
		list []domain.LocalPlugin
		want string
	}{
		{"missing path", []domain.LocalPlugin{{Path: filepath.Join(pc.RunRoot, "..", "gone"), Required: true}}, "gone"},
		{"relative path", []domain.LocalPlugin{{Path: "plugins/guard", Required: true}}, "absolute"},
		{"a file", []domain.LocalPlugin{{Path: aFile, Required: true}}, "not a directory"},
		{"no manifest", []domain.LocalPlugin{{Path: t.TempDir(), Required: true}}, "plugin.json"},
		{"duplicate", []domain.LocalPlugin{{Path: dup}, {Path: dup, Required: true}}, "same directory"},
		{"bad grant", []domain.LocalPlugin{{Path: newPluginDir(t), Required: true, GrantTools: []string{"Skill(x)"}}}, "per-skill"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plugins, warnings, err := PreparePlugins(c.list, pc)
			if err == nil {
				t.Fatalf("PreparePlugins = %v, %v, nil; want an error", plugins, warnings)
			}
			if !errors.Is(err, ErrPluginNotLoaded) {
				t.Errorf("error %v does not wrap ErrPluginNotLoaded", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			if plugins != nil || warnings != nil {
				t.Errorf("a failed spawn returned plugins %v and warnings %v", plugins, warnings)
			}
		})
	}
}

func TestPreparePlugins_NonRequiredEntryIsSkippedWithAWarning(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	good := newPluginDir(t)
	plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{
		{Path: filepath.Join(pc.RunRoot, "..", "gone")},
		{Path: good},
		{Path: good, GrantTools: []string{"Bash"}},
	}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Path != good {
		t.Fatalf("plugins = %+v, want only the good entry", plugins)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2", warnings)
	}
	if warnings[0].Field != "local_plugins[0].path" || !strings.Contains(warnings[0].Message, "cannot be read") || warnings[0].EnvKeyDropped {
		t.Errorf("warning 0 = %+v", warnings[0])
	}
	// The duplicate is refused before its grants are looked at.
	if warnings[1].Field != "local_plugins[2].path" || !strings.Contains(warnings[1].Message, "same directory") {
		t.Errorf("warning 1 = %+v", warnings[1])
	}
}

func TestPreparePlugins_BadGrantOnNonRequiredEntryIsSkipped(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{
		{Path: newPluginDir(t), GrantTools: []string{"Skill(x)"}},
	}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("plugins = %+v, want none", plugins)
	}
	if len(warnings) != 1 || warnings[0].Field != "local_plugins[0].grant_tools[0]" || !strings.Contains(warnings[0].Message, "per-skill") {
		t.Errorf("warnings = %+v", warnings)
	}
	// An entry left out has no state directory.
	if _, err := os.Stat(filepath.Join(pc.RunRoot, "_tfac", "plugins", "0")); !os.IsNotExist(err) {
		t.Errorf("state dir exists for a skipped entry (err=%v)", err)
	}
}

func TestPreparePlugins_RefusesAPluginTheAgentCanWrite(t *testing.T) {
	isolatePluginEnv(t)

	inside := func(t *testing.T, base string) string {
		t.Helper()
		dir := filepath.Join(base, "nested", "plugin")
		manifest := filepath.Join(dir, pluginManifest)
		if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifest, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	cases := []struct {
		name string
		pick func(pc PluginPrepareContext) string // the tree the plugin sits in
	}{
		{"the run root", func(pc PluginPrepareContext) string { return pc.RunRoot }},
		{"an add-dir", func(pc PluginPrepareContext) string { return pc.AddDirs[0] }},
		{"the runs base", func(pc PluginPrepareContext) string { return pc.RunsBase }},
	}
	for _, c := range cases {
		t.Run(c.name+" required", func(t *testing.T) {
			pc := prepareCtx(t)
			dir := inside(t, c.pick(pc))
			_, _, err := PreparePlugins([]domain.LocalPlugin{{Path: dir, Required: true}}, pc)
			if err == nil || !errors.Is(err, ErrPluginNotLoaded) {
				t.Fatalf("err = %v, want ErrPluginNotLoaded", err)
			}
			if !strings.Contains(err.Error(), "agent can write") {
				t.Errorf("err = %q, want it to say why", err)
			}
		})
		t.Run(c.name+" not required", func(t *testing.T) {
			pc := prepareCtx(t)
			dir := inside(t, c.pick(pc))
			plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{{Path: dir}}, pc)
			if err != nil {
				t.Fatalf("PreparePlugins: %v", err)
			}
			if len(plugins) != 0 || len(warnings) != 1 || warnings[0].Field != "local_plugins[0].path" {
				t.Errorf("plugins = %+v, warnings = %+v; want the entry skipped with one path warning", plugins, warnings)
			}
		})
	}

	t.Run("the root itself", func(t *testing.T) {
		pc := prepareCtx(t)
		manifest := filepath.Join(pc.RunRoot, pluginManifest)
		if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifest, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := PreparePlugins([]domain.LocalPlugin{{Path: pc.RunRoot, Required: true}}, pc)
		if !errors.Is(err, ErrPluginNotLoaded) {
			t.Errorf("err = %v, want ErrPluginNotLoaded for a plugin that is the run root", err)
		}
	})
}

func TestPreparePlugins_DropsADisallowedEnvKeyAndStillLoads(t *testing.T) {
	isolatePluginEnv(t)
	pc := prepareCtx(t)
	plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{{
		Path: newPluginDir(t),
		Env: map[string]string{
			"GOOD":      "1",
			"TF_BAD":    "x",
			"BAD_VALUE": "{nope}",
			"MY_TOKEN":  "x",
		},
	}}, pc)
	if err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if len(plugins) != 1 || !slices.Equal(plugins[0].Env, []string{"GOOD=1"}) {
		t.Fatalf("plugins = %+v, want one entry carrying only GOOD=1", plugins)
	}
	got := map[string]PluginWarning{}
	for _, w := range warnings {
		got[w.Field] = w
	}
	for _, field := range []string{
		"local_plugins[0].env.TF_BAD",
		"local_plugins[0].env.BAD_VALUE",
		"local_plugins[0].env.MY_TOKEN",
	} {
		w, ok := got[field]
		if !ok {
			t.Errorf("no warning for %s; got %+v", field, warnings)
			continue
		}
		if !w.EnvKeyDropped {
			t.Errorf("warning for %s has EnvKeyDropped = false", field)
		}
	}
	if len(warnings) != 3 {
		t.Errorf("warnings = %+v, want 3", warnings)
	}
}

func TestPreparePlugins_ListOverTheCapFailsTheSpawn(t *testing.T) {
	isolatePluginEnv(t)
	list := make([]domain.LocalPlugin, MaxLocalPlugins+1)
	for i := range list {
		list[i] = domain.LocalPlugin{Path: "/does/not/matter"}
	}
	_, _, err := PreparePlugins(list, prepareCtx(t))
	if !errors.Is(err, ErrPluginNotLoaded) {
		t.Fatalf("err = %v, want ErrPluginNotLoaded", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxLocalPlugins+1)) {
		t.Errorf("err = %q, want the entry count", err)
	}
}

func TestPreparePlugins_RunRootMustBeAbsolute(t *testing.T) {
	isolatePluginEnv(t)
	list := []domain.LocalPlugin{{Path: newPluginDir(t)}}
	for _, root := range []string{"", "relative/run"} {
		_, _, err := PreparePlugins(list, PluginPrepareContext{RunRoot: root})
		if !errors.Is(err, ErrPluginNotLoaded) {
			t.Errorf("RunRoot %q: err = %v, want ErrPluginNotLoaded", root, err)
		}
	}
}

func TestPreparePlugins_RunsTheSpawnPrefixCheck(t *testing.T) {
	isolatePluginEnv(t)
	calls := 0
	verifyPluginPrefixesForSpawn = func() { calls++ }
	if _, _, err := PreparePlugins([]domain.LocalPlugin{{Path: newPluginDir(t)}}, prepareCtx(t)); err != nil {
		t.Fatalf("PreparePlugins: %v", err)
	}
	if calls != 1 {
		t.Errorf("spawn prefix check ran %d times, want 1", calls)
	}
}

func TestInsideAny(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	cases := []struct {
		name  string
		path  string
		bases []string
		want  string
		found bool
	}{
		{"the base itself", "/runs/a", []string{"/runs/a"}, "/runs/a", true},
		{"a child", "/runs/a/p/q", []string{"/runs/a"}, "/runs/a", true},
		{"a sibling sharing a name prefix", "/runs/ab", []string{"/runs/a"}, "", false},
		{"a parent of the base", "/runs", []string{"/runs/a"}, "", false},
		{"unrelated", "/opt/plugin", []string{"/runs/a", "/srv"}, "", false},
		{"the first match is named", "/runs/a/p", []string{"/other", "/runs", "/runs/a"}, "/runs", true},
		{"blank bases are skipped", "/runs/a", []string{"", "  "}, "", false},
		{"no bases", "/runs/a", nil, "", false},
		{"an unclean base", "/runs/a/p", []string{"/runs//a/../a/"}, "/runs//a/../a/", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, found := insideAny(c.path, c.bases)
			if got != c.want || found != c.found {
				t.Errorf("insideAny(%q, %v) = %q, %v; want %q, %v", c.path, c.bases, got, found, c.want, c.found)
			}
		})
	}
}

func TestInsideAny_ResolvesASymlinkedBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege on Windows")
	}
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// A plugin path arrives resolved; the base the run was configured with may
	// be spelled through a link.
	got, found := insideAny(filepath.Join(real, "plugin"), []string{link})
	if !found || got != link {
		t.Errorf("insideAny = %q, %v; want %q, true", got, found, link)
	}
}

func TestPluginEnvForSpawn(t *testing.T) {
	isolatePluginEnv(t)
	plugins := []Plugin{
		{Env: []string{"FIRST=1", "COLLIDES=plugin", "URL=a=b"}},
		{Env: []string{"SECOND=2", "GIT_CONFIG_COUNT=9"}},
		{Env: []string{"TF_REFUSED=1", "NOEQUALS", "=novalue", "LAST=3"}},
	}
	tfSet := []string{"COLLIDES=tf", "UNRELATED=x", "GIT_CONFIG_COUNT=", "malformed"}

	type drop struct{ key, reason string }
	var drops []drop
	got := pluginEnvForSpawn(plugins, tfSet, func(key, reason string) { drops = append(drops, drop{key, reason}) })

	want := []string{"FIRST=1", "URL=a=b", "SECOND=2", "LAST=3"}
	if !slices.Equal(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
	if len(drops) != 3 {
		t.Fatalf("drops = %v, want 3", drops)
	}
	for i, wantKey := range []string{"COLLIDES", "GIT_CONFIG_COUNT", "TF_REFUSED"} {
		if drops[i].key != wantKey {
			t.Errorf("drop %d key = %q, want %q", i, drops[i].key, wantKey)
		}
	}
	if !strings.Contains(drops[0].reason, "Triage Factory sets this key") {
		t.Errorf("collision reason = %q", drops[0].reason)
	}
	if !strings.Contains(drops[2].reason, "reserved") {
		t.Errorf("refused-key reason = %q", drops[2].reason)
	}
}

func TestPluginEnvForSpawn_EdgeCases(t *testing.T) {
	isolatePluginEnv(t)
	if got := pluginEnvForSpawn(nil, []string{"A=1"}, nil); got != nil {
		t.Errorf("no plugins: got %v, want nil", got)
	}
	if got := pluginEnvForSpawn([]Plugin{{}}, nil, nil); got != nil {
		t.Errorf("a plugin with no env: got %v, want nil", got)
	}
	// A nil drop callback is allowed and the colliding key is still left out.
	got := pluginEnvForSpawn([]Plugin{{Env: []string{"A=plugin", "B=2"}}}, []string{"A=tf"}, nil)
	if !slices.Equal(got, []string{"B=2"}) {
		t.Errorf("got %v, want [B=2]", got)
	}
}

func TestPluginInitTimeout(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv(envPluginInitTimeout, "")
		if err := os.Unsetenv(envPluginInitTimeout); err != nil {
			t.Fatal(err)
		}
		got, err := PluginInitTimeout()
		if err != nil || got != 60*time.Second {
			t.Errorf("= %v, %v; want 60s", got, err)
		}
	})
	t.Run("blank", func(t *testing.T) {
		t.Setenv(envPluginInitTimeout, "  ")
		got, err := PluginInitTimeout()
		if err != nil || got != 60*time.Second {
			t.Errorf("= %v, %v; want 60s", got, err)
		}
	})
	for raw, want := range map[string]time.Duration{"5": 5 * time.Second, " 7 ": 7 * time.Second, "120": 2 * time.Minute} {
		t.Run("value "+raw, func(t *testing.T) {
			t.Setenv(envPluginInitTimeout, raw)
			got, err := PluginInitTimeout()
			if err != nil || got != want {
				t.Errorf("= %v, %v; want %v", got, err, want)
			}
		})
	}
	for _, raw := range []string{"0", "-1", "abc", "1.5", "5s", "1e3"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			t.Setenv(envPluginInitTimeout, raw)
			got, err := PluginInitTimeout()
			if err == nil {
				t.Fatalf("= %v, nil; want an error", got)
			}
			if !strings.Contains(err.Error(), envPluginInitTimeout) {
				t.Errorf("error %q does not name %s", err, envPluginInitTimeout)
			}
		})
	}
}

// TestPluginEnvKeyAllowed_RefusesCredentialResolverKeys: a key the credential
// resolver strips from a run's env picks the run's model provider or account,
// so a plugin cannot set it, even where no other family covers it.
func TestPluginEnvKeyAllowed_RefusesCredentialResolverKeys(t *testing.T) {
	for _, key := range []string{"CLOUD_ML_REGION", "GCLOUD_PROJECT", "VERTEX_REGION_CLAUDE_X"} {
		if !isCredentialEnvKey(key) {
			t.Fatalf("%s is not a credential resolver key; the test's premise moved", key)
		}
		if ok, reason := PluginEnvKeyAllowed(key); ok {
			t.Errorf("PluginEnvKeyAllowed(%q) = true, want refused", key)
		} else if reason == "" {
			t.Errorf("PluginEnvKeyAllowed(%q) refused with no reason", key)
		}
	}
}

// TestPluginEnvKeyAllowed_RefusesRunWideRedirects: a plugin's env reaches the
// whole run, so keys that redirect a shell, an interpreter, a package fetch or
// TLS key logging for the agent's own commands are refused too.
func TestPluginEnvKeyAllowed_RefusesRunWideRedirects(t *testing.T) {
	for _, key := range []string{
		"SHELL", "ZDOTDIR", "PERLLIB", "LUA_INIT", "GLIBC_TUNABLES", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS",
		"NODE_USE_ENV_PROXY", "NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "UV_INDEX_URL", "CARGO_HOME",
		"GOPROXY", "GOFLAGS", "GOSUMDB", "GOTOOLCHAIN", "SSLKEYLOGFILE",
	} {
		if ok, _ := PluginEnvKeyAllowed(key); ok {
			t.Errorf("PluginEnvKeyAllowed(%q) = true, want refused", key)
		}
	}
}

// TestPreparePlugins_RefusesASymlinkedStateDir: a symlink where a state
// directory goes, left by an earlier engagement's agent, fails a required
// entry and skips an optional one, and the link's target is not touched.
func TestPreparePlugins_RefusesASymlinkedStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	plugin := makePluginDir(t, "guard")
	for _, required := range []bool{true, false} {
		runRoot := t.TempDir()
		target := t.TempDir()
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(runRoot, "_tfac", "plugins"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(runRoot, "_tfac", "plugins", "0")); err != nil {
			t.Fatal(err)
		}
		plugins, warnings, err := PreparePlugins([]domain.LocalPlugin{{Path: plugin, Required: required}}, PluginPrepareContext{RunRoot: runRoot})
		if required {
			if !errors.Is(err, ErrPluginNotLoaded) || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("required: err = %v, want a symlink refusal", err)
			}
		} else if err != nil || len(plugins) != 0 || len(warnings) != 1 {
			t.Fatalf("optional: plugins=%v warnings=%v err=%v, want it skipped with a warning", plugins, warnings, err)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("the link target's mode changed: %v %v", info.Mode(), err)
		}
	}
}
