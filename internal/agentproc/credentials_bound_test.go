package agentproc

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// keychainBag stands in for local mode's keychain: one bag of keys shared by
// every state root, read through the system door.
type keychainBag struct {
	values map[string]string
	err    error
	reads  []string
}

func (b *keychainBag) GetSystem(_ context.Context, _, key string) (string, error) {
	b.reads = append(b.reads, key)
	if b.err != nil {
		return "", b.err
	}
	return b.values[key], nil
}

// boundTo is a binding record naming providers, as the settings row's refs do.
func boundTo(providers ...string) func(context.Context, string) (map[string]bool, error) {
	return func(context.Context, string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, p := range providers {
			out[p] = true
		}
		return out, nil
	}
}

// A local run whose org bound an Anthropic key runs on that key, and nothing
// the operator's environment or Claude Code settings carry reaches it: not a
// key or OAuth token, not a model pin, not another provider's configuration,
// not a settings helper, and not the claude.ai connectors that ride the
// operator's login.
func TestNewDirectCommand_LocalRunAuthenticatesWithTheBoundKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	writeOperatorSettings(t, operatorSettingsFixture)
	inherited := map[string]string{
		"ANTHROPIC_API_KEY":       "sk-operator-shell",
		"CLAUDE_CODE_OAUTH_TOKEN": "operator-subscription",
		"ANTHROPIC_MODEL":         "claude-operator-pin",
		"ANTHROPIC_BASE_URL":      "https://operator-gateway.example",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_PROFILE":             "operator-account",
	}
	for k, v := range inherited {
		t.Setenv(k, v)
	}
	bag := &keychainBag{values: map[string]string{"anthropic_api_key": "sk-ant-bound"}}

	cmd, err := newDirectCommand(context.Background(), RunOptions{
		Cwd:                t.TempDir(),
		OrgID:              runmode.LocalDefaultOrgID,
		Model:              "sonnet",
		Secrets:            NewBoundSecretsReader(bag, boundTo(modelcatalog.ProviderAnthropic)),
		ClaudeAIConnectors: true,
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}

	env := envMap(t, cmd.Env)
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-ant-bound" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the bound key", got)
	}
	for k := range inherited {
		if k == "ANTHROPIC_API_KEY" {
			continue
		}
		if v, ok := env[k]; ok {
			t.Errorf("inherited %s=%q reached a run on the bound key", k, v)
		}
	}
	for _, flag := range []string{"--settings", "--claude-ai-connectors"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("a run on the bound key carries %s: %v", flag, cmd.Args)
		}
	}
}

// The settings row, not the keychain, decides which providers a local org
// configured. The keychain is one bag for every state root of the OS user, so it
// can hold material this org never bound here.
func TestResolveCredentials_LocalBindingRecordDecides(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	org := runmode.LocalDefaultOrgID
	stale := map[string]string{"anthropic_api_key": "sk-ant-left-by-another-state-root"}

	t.Run("an org on the host's credentials reads nothing", func(t *testing.T) {
		bag := &keychainBag{values: stale}
		env, err := resolveCredentials(context.Background(), NewBoundSecretsReader(bag, boundTo()), org, "sonnet", nil)
		if err != nil {
			t.Fatalf("resolveCredentials: %v", err)
		}
		if len(env) != 0 {
			t.Errorf("env = %v, want empty so the run inherits the host's credentials", env)
		}
		if len(bag.reads) != 0 {
			t.Errorf("read %v from the keychain for an org that bound nothing", bag.reads)
		}
	})

	t.Run("a stray Anthropic key does not outrank the bound Bedrock token", func(t *testing.T) {
		bag := &keychainBag{values: map[string]string{
			"anthropic_api_key":        stale["anthropic_api_key"],
			"aws_bearer_token_bedrock": "bedrock-bound",
			"aws_region":               "us-west-2",
		}}
		env, err := resolveCredentials(context.Background(), NewBoundSecretsReader(bag, boundTo(modelcatalog.ProviderBedrock)), org, "sonnet", nil)
		if err != nil {
			t.Fatalf("resolveCredentials: %v", err)
		}
		if env["AWS_BEARER_TOKEN_BEDROCK"] != "bedrock-bound" || env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
			t.Errorf("env = %v, want the bound Bedrock token", env)
		}
		if _, ok := env["ANTHROPIC_API_KEY"]; ok {
			t.Errorf("the unbound Anthropic key reached the run: %v", env)
		}
	})

	t.Run("a bound key missing from the keychain fails the run", func(t *testing.T) {
		bag := &keychainBag{values: map[string]string{}}
		_, err := resolveCredentials(context.Background(), NewBoundSecretsReader(bag, boundTo(modelcatalog.ProviderAnthropic)), org, "sonnet", nil)
		if !errors.Is(err, ErrProviderNotConfigured) {
			t.Errorf("err = %v, want ErrProviderNotConfigured rather than a fall back to the host's credentials", err)
		}
	})

	t.Run("a keychain read error fails the run", func(t *testing.T) {
		want := errors.New("keychain locked")
		bag := &keychainBag{err: want}
		_, err := resolveCredentials(context.Background(), NewBoundSecretsReader(bag, boundTo(modelcatalog.ProviderAnthropic)), org, "sonnet", nil)
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want the keychain's error", err)
		}
	})

	t.Run("an unreadable binding record fails the run", func(t *testing.T) {
		want := errors.New("database is locked")
		bag := &keychainBag{values: stale}
		unreadable := func(context.Context, string) (map[string]bool, error) { return nil, want }
		_, err := resolveCredentials(context.Background(), NewBoundSecretsReader(bag, unreadable), org, "sonnet", nil)
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want the settings read's error", err)
		}
		if len(bag.reads) != 0 {
			t.Errorf("read %v from the keychain without a binding record", bag.reads)
		}
	})
}
