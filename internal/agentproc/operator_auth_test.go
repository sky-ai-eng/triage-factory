package agentproc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// operatorSettingsFixture is a user settings file that sets every kind of key a
// run must not inherit (permission rules, hooks, plugins, a status line, proxy
// and behavior env) beside the auth surface it must.
const operatorSettingsFixture = `{
  "permissions": {"allow": ["Bash(curl:*)", "WebFetch"], "deny": ["Bash(git push *)"]},
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "~/.claude/hooks/start.sh"}]}]},
  "enabledPlugins": {"tool@market": true},
  "statusLine": {"type": "command", "command": "status.sh"},
  "apiKeyHelper": "/usr/local/bin/key-helper",
  "awsAuthRefresh": "aws sso login --profile work",
  "gcpAuthRefresh": 5,
  "proxyAuthHelper": "",
  "env": {
    "ANTHROPIC_BASE_URL": "https://gateway.example",
    "CLAUDE_CODE_USE_BEDROCK": 1,
    "AWS_PROFILE": "work",
    "AWS_REGION": {"nested": "dropped"},
    "HTTPS_PROXY": "http://proxy.example:3128",
    "NODE_EXTRA_CA_CERTS": "/etc/ssl/corp.pem",
    "BASH_DEFAULT_TIMEOUT_MS": "1"
  }
}`

func writeOperatorSettings(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	writeFile(t, path, content)
	return path
}

// TestReadOperatorAuth_ForwardsOnlyTheAuthSurface pins what a run without a
// resolved credential still takes from the operator's settings: the helper
// commands that are set to a command, and the env keys credentialEnvKeys
// names, rendered the way Claude Code renders them. Everything else in the
// file, permission rules and hooks above all, stays behind.
func TestReadOperatorAuth_ForwardsOnlyTheAuthSurface(t *testing.T) {
	auth, err := readOperatorAuth(writeOperatorSettings(t, operatorSettingsFixture))
	if err != nil {
		t.Fatalf("readOperatorAuth: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal([]byte(auth.settings), &settings); err != nil {
		t.Fatalf("settings %q is not a JSON object: %v", auth.settings, err)
	}
	wantSettings := map[string]any{
		"apiKeyHelper":   "/usr/local/bin/key-helper",
		"awsAuthRefresh": "aws sso login --profile work",
	}
	if len(settings) != len(wantSettings) {
		t.Errorf("settings = %v, want exactly %v", settings, wantSettings)
	}
	for k, v := range wantSettings {
		if settings[k] != v {
			t.Errorf("settings[%s] = %v, want %v", k, settings[k], v)
		}
	}

	wantEnv := []string{
		"ANTHROPIC_BASE_URL=https://gateway.example",
		"AWS_PROFILE=work",
		"CLAUDE_CODE_USE_BEDROCK=1",
	}
	if !slices.Equal(auth.env, wantEnv) {
		t.Errorf("env = %v, want %v", auth.env, wantEnv)
	}
}

// TestReadOperatorAuth_ForwardsEveryProviderCategory pins the forward job
// across the provider surface: a Vertex, Foundry, Anthropic-on-cloud or
// gateway setup that lives in the settings env authenticates the same way
// under TF as it does in Claude Code, a per-model VERTEX_REGION_ name
// included.
func TestReadOperatorAuth_ForwardsEveryProviderCategory(t *testing.T) {
	env := map[string]string{}
	var want []string
	for _, kv := range providerEnvByCategory {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
		want = append(want, kv)
	}
	for _, kv := range providerEnvPassthrough {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	body, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		t.Fatal(err)
	}

	auth, err := readOperatorAuth(writeOperatorSettings(t, string(body)))
	if err != nil {
		t.Fatalf("readOperatorAuth: %v", err)
	}
	slices.Sort(want)
	if !slices.Equal(auth.env, want) {
		t.Errorf("env = %v, want %v", auth.env, want)
	}
}

func TestReadOperatorAuth_MissingFileForwardsNothing(t *testing.T) {
	auth, err := readOperatorAuth(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("readOperatorAuth: %v", err)
	}
	if auth.settings != "" || auth.env != nil {
		t.Errorf("auth = %+v, want nothing forwarded", auth)
	}
}

func TestReadOperatorAuth_MalformedFileIsAnError(t *testing.T) {
	if _, err := readOperatorAuth(writeOperatorSettings(t, `{"apiKeyHelper": `)); err == nil {
		t.Error("readOperatorAuth accepted a truncated settings file")
	}
}

// TestNewDirectCommand_ForwardsOperatorAuthWithoutACredential pins the
// "Use system Claude Code credentials" spawn: the helper commands ride
// --settings after the wrapper's own flags, and the credential env rides the
// process env over whatever the shell exported, the precedence a settings env
// has over the process env inside Claude Code.
func TestNewDirectCommand_ForwardsOperatorAuthWithoutACredential(t *testing.T) {
	writeOperatorSettings(t, operatorSettingsFixture)
	t.Setenv("ANTHROPIC_BASE_URL", "https://from-the-shell.example")

	cmd, err := newDirectCommand(context.Background(), RunOptions{Cwd: t.TempDir()}, []string{"wrapper.mjs", "--model", "x"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}

	idx := slices.Index(cmd.Args, "--settings")
	if idx < 0 || idx+1 >= len(cmd.Args) {
		t.Fatalf("argv carries no --settings: %v", cmd.Args)
	}
	if !slices.Equal(cmd.Args[:idx], []string{"node", "wrapper.mjs", "--model", "x"}) {
		t.Errorf("--settings is not appended after the wrapper args: %v", cmd.Args)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(cmd.Args[idx+1]), &settings); err != nil {
		t.Fatalf("--settings %q: %v", cmd.Args[idx+1], err)
	}
	for _, key := range []string{"permissions", "hooks", "enabledPlugins", "statusLine", "env"} {
		if _, ok := settings[key]; ok {
			t.Errorf("--settings carries %q: %v", key, settings)
		}
	}

	env := envMap(t, cmd.Env)
	if got := env["ANTHROPIC_BASE_URL"]; got != "https://gateway.example" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the settings value over the shell's", got)
	}
	if got := env["CLAUDE_CODE_USE_BEDROCK"]; got != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1", got)
	}
	// Compared by value: the test runner's own environment may set these.
	for key, fromSettings := range map[string]string{
		"HTTPS_PROXY":             "http://proxy.example:3128",
		"NODE_EXTRA_CA_CERTS":     "/etc/ssl/corp.pem",
		"BASH_DEFAULT_TIMEOUT_MS": "1",
	} {
		if env[key] == fromSettings {
			t.Errorf("%s from the settings file reached the run's env", key)
		}
	}
}

// TestNewDirectCommand_StoredCredentialForwardsNothing pins the stored-key
// guarantee: with a credential TF resolved, nothing from the operator's
// settings reaches the spawn, so neither a helper nor a settings env can
// replace the key the org entered.
func TestNewDirectCommand_StoredCredentialForwardsNothing(t *testing.T) {
	writeOperatorSettings(t, `{
  "apiKeyHelper": "/usr/local/bin/key-helper",
  "env": {"ANTHROPIC_API_KEY": "sk-from-settings", "ANTHROPIC_BASE_URL": "https://gateway.example"}
}`)
	resolver := func(context.Context, string, string) (map[string]string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-stored"}, nil
	}

	cmd, err := newDirectCommand(context.Background(), RunOptions{Cwd: t.TempDir(), LLMResolver: resolver}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	if slices.Contains(cmd.Args, "--settings") {
		t.Errorf("a run with a stored credential forwards --settings: %v", cmd.Args)
	}
	env := envMap(t, cmd.Env)
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-stored" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the stored key", got)
	}
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("ANTHROPIC_BASE_URL reached a run with a stored credential: %q", env["ANTHROPIC_BASE_URL"])
	}
}
