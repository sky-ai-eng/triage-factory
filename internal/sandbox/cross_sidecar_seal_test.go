package sandbox

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
)

// TestCrossSidecarSealedBundleIsolation is the executable form of the first
// cross-sidecar negative: sidecar A cannot read sidecar B's sealed credential
// row. Each run's sidecar mints its OWN X25519 keypair (newCredRuntime, one per
// run) and the brain seals that run's bundle to that run's public key; the
// private half is born in the sidecar and never leaves it. So the boundary is
// the seal itself — A holds a different private key and simply cannot open a
// row sealed to B.
//
// No live process is needed to prove this: it is a property of credseal's
// box seal, which the whole per-run scheme rests on. The live two-run harness
// (eviction_linux_test.go) proves the *processes* are distinct; this proves the
// *ciphertext* one sidecar could ever observe is unreadable to another.
//
// Untagged (runs under plain `go test`): credseal/credbundle are
// cross-platform, so this negative is verifiable without a runsc host, unlike
// the socket/memory negatives that need real sandboxes.
func TestCrossSidecarSealedBundleIsolation(t *testing.T) {
	// Run A and run B each mint their own per-run keypair.
	kpA, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("mint run A keypair: %v", err)
	}
	kpB, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("mint run B keypair: %v", err)
	}
	if kpA.Public == kpB.Public {
		t.Fatal("two freshly-minted per-run keypairs share a public key — the seal target is not per-run")
	}

	// Run B's bundle carries the full sealed set: LLM + a provider (Slack)
	// credential under bundle.Providers. The seal-per-run-pubkey property covers
	// the whole bundle — provider creds included — so exercising a provider key
	// here guards the larger post-relocation bundle, not just the LLM triple.
	bundleB := &credbundle.Bundle{
		LLM:       map[string]string{"ANTHROPIC_API_KEY": "sk-ant-run-B-secret"},
		Providers: map[string]json.RawMessage{"slack": json.RawMessage(`{"bot_token":"xoxb-run-B-secret"}`)},
	}
	plaintextB, err := bundleB.Marshal()
	if err != nil {
		t.Fatalf("marshal run B bundle: %v", err)
	}

	// The brain seals run B's row to run B's public key.
	sealedForB, err := credseal.Seal(kpB.Public, plaintextB)
	if err != nil {
		t.Fatalf("seal run B bundle to run B pubkey: %v", err)
	}

	// The negative: run A's sidecar, holding only its own private key, cannot
	// open run B's row.
	if _, err := kpA.Open(sealedForB); err == nil {
		t.Fatal("CROSS-SIDECAR CREDENTIAL LEAK: run A's key opened a bundle sealed to run B — the per-run seal is not isolating")
	}

	// The positive control: run B's own key opens its own row and recovers the
	// exact bytes — so the failure above is "wrong key", not "seal is unopenable
	// by anyone" (which would make the negative vacuous).
	got, err := kpB.Open(sealedForB)
	if err != nil {
		t.Fatalf("run B's own key failed to open run B's bundle: %v", err)
	}
	if !bytes.Equal(got, plaintextB) {
		t.Fatal("run B opened its own bundle but the plaintext did not round-trip")
	}
}
