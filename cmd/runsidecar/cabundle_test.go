package runsidecar

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// TestRuntime_StartProxiesWritesTheCABundle pins the jail's CA bundle as a
// step of its own: a run with no gh channel still gets one, and it carries the
// egress interceptor's CA, without which no registry the proxy admits
// verifies in the jail. With the gh channel on, it carries the injector's
// leaf as well.
func TestRuntime_StartProxiesWritesTheCABundle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ghWant bool
	}{
		{"without a gh channel", false},
		{"with a gh channel", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, rt := dialRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			bundle := &credbundle.Bundle{
				LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
				GitHub: &credbundle.GitHubCreds{
					Mode:     "app",
					BaseURL:  "https://github.com",
					CLIToken: &credbundle.RepoToken{Token: "ghs_REALCLITOKEN"},
				},
			}
			if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
				sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
				t.Fatalf("relay bundle: %v", err)
			}

			written := map[string][]byte{}
			origWrite, origStart := writeCABundle, startAgentHostSocket
			writeCABundle = func(conversationID string, pem []byte) error {
				written[conversationID] = pem
				return nil
			}
			startAgentHostSocket = func(*agenthost.Server, string) (*agenthost.HostDaemon, sandbox.Mount, error) {
				return nil, sandbox.Mount{}, nil
			}
			t.Cleanup(func() { writeCABundle, startAgentHostSocket = origWrite, origStart })

			var res sidecarproto.StartProxiesResult
			if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
				HostVethIP:       "127.0.0.1",
				GHChannelEnabled: tc.ghWant,
				AgentHost:        &sidecarproto.AgentHostInfo{OrgID: "org", ConversationID: "run-ca"},
			}, &res); err != nil {
				t.Fatalf("start proxies: %v", err)
			}
			t.Cleanup(func() { _ = rt.proxies.Shutdown(context.Background()) })

			raw, ok := written["run-ca"]
			if !ok {
				t.Fatal("no CA bundle written for the run")
			}
			var interceptCA, ghLeaf bool
			for rest := raw; ; {
				var block *pem.Block
				block, rest = pem.Decode(rest)
				if block == nil {
					break
				}
				c, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					t.Fatalf("bundle holds an unparsable certificate: %v", err)
				}
				if c.IsCA && slices.Contains(c.PermittedDNSDomains, "registry.npmjs.org") {
					interceptCA = true
				}
				if c.Subject.CommonName == "triage-factory gh injector" {
					ghLeaf = true
				}
			}
			if !interceptCA {
				t.Error("CA bundle lacks the egress interceptor's CA; every admitted registry would fail verification in the jail")
			}
			if ghLeaf != tc.ghWant {
				t.Errorf("gh injector leaf in the bundle = %v, want %v", ghLeaf, tc.ghWant)
			}
		})
	}
}
