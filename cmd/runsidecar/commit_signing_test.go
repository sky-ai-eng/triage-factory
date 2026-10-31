package runsidecar

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// signingStart relays bundle to a fresh sidecar and starts its proxies for a
// run with an exec-verb daemon, returning the result, the jail's git config
// and the allowed-signers file the sidecar wrote.
func signingStart(t *testing.T, bundle *credbundle.Bundle, daemon bool) (sidecarproto.StartProxiesResult, map[string]string, string) {
	t.Helper()
	dir := stubPrivilegedSideEffects(t)
	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}
	body := sidecarproto.StartProxiesBody{HostVethIP: "127.0.0.1"}
	if daemon {
		body.AgentHost = &sidecarproto.AgentHostInfo{OrgID: "org", ConversationID: "run-sign"}
	}
	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, body, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}
	t.Cleanup(func() { rt.shutdown(context.Background()) })
	signers, err := os.ReadFile(filepath.Join(dir, "run-sign.allowed-signers"))
	if daemon && err != nil {
		t.Fatalf("no allowed-signers file written for the run: %v", err)
	}
	return res, gitConfigFromEnv(t, res.Env), string(signers)
}

func signerBundle(t *testing.T, signer *credbundle.SignerCreds) *credbundle.Bundle {
	t.Helper()
	return &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
		GitHub: &credbundle.GitHubCreds{
			Mode:              credbundle.GitHubModeApp,
			BaseURL:           "https://github.com",
			IdentityName:      "Acme Agent",
			IdentityEmail:     "583231+acme-agent@users.noreply.github.com",
			IdentityAccountID: "583231",
			Signer:            signer,
		},
	}
}

func publicKey(t *testing.T) string {
	t.Helper()
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := commitsign.PublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return public
}

// TestRuntime_StartProxiesConfiguresTheJailToSign: a bundle naming an active
// signer has the jail's git sign every commit as it, through the shim in the
// hooks mount, checked against the allowed-signers file at its in-jail path,
// which lists the signer's current and pending keys. The result reports the
// kind of signer for the orchestrator's push accounting.
func TestRuntime_StartProxiesConfiguresTheJailToSign(t *testing.T) {
	current, pending := publicKey(t), publicKey(t)
	res, cfg, signers := signingStart(t, signerBundle(t, &credbundle.SignerCreds{
		Mode: "org", PublicKey: current, PendingPublicKey: pending,
	}), true)

	want := map[string]string{
		"user.name":                  "Acme Agent",
		"user.email":                 "583231+acme-agent@users.noreply.github.com",
		"gpg.format":                 "ssh",
		"commit.gpgsign":             "true",
		"user.signingkey":            "key::" + current,
		"gpg.ssh.program":            githooks.SandboxDir + "/" + githooks.SignProgram,
		"gpg.ssh.allowedSignersFile": cell.AllowedSignersPath,
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("jail git config %s = %q, want %q", k, cfg[k], v)
		}
	}
	if res.CommitSigner != "org" {
		t.Errorf("CommitSigner = %q, want org", res.CommitSigner)
	}
	if got, want := signers, string(githooks.AllowedSigners("583231+acme-agent@users.noreply.github.com", current, pending)); got != want {
		t.Errorf("allowed-signers file = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(res.Env, "\n"), "PRIVATE KEY") {
		t.Error("the jail's env carries a private key")
	}
}

// TestRuntime_StartProxiesLeavesAnUnsignedRunUnsigned: a bundle naming no
// signer, or a run with no exec-verb daemon to answer git's signing program,
// is not configured to sign, and its allowed-signers file is empty.
func TestRuntime_StartProxiesLeavesAnUnsignedRunUnsigned(t *testing.T) {
	signer := &credbundle.SignerCreds{Mode: "org", PublicKey: publicKey(t)}
	for _, c := range []struct {
		name   string
		bundle *credbundle.Bundle
		daemon bool
	}{
		{"no signer", signerBundle(t, nil), true},
		{"no daemon", signerBundle(t, signer), false},
		{"not a signing mode", signerBundle(t, &credbundle.SignerCreds{Mode: "none", PublicKey: signer.PublicKey}), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, cfg, signers := signingStart(t, c.bundle, c.daemon)
			for _, k := range []string{"gpg.format", "commit.gpgsign", "user.signingkey", "gpg.ssh.program", "gpg.ssh.allowedSignersFile"} {
				if v, ok := cfg[k]; ok {
					t.Errorf("jail git config sets %s = %q for a run that does not sign", k, v)
				}
			}
			if cfg["user.name"] != "Acme Agent" {
				t.Errorf("user.name = %q, want the bundle's identity, which a run that does not sign still commits as", cfg["user.name"])
			}
			if res.CommitSigner != "" || signers != "" {
				t.Errorf("CommitSigner = %q, allowed-signers %q; want neither", res.CommitSigner, signers)
			}
		})
	}
}
