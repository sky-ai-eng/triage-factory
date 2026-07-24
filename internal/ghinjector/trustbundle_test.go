package ghinjector

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
)

// TestTrustBundlePEM_ParsesAsPoolWithLeaf is the property that actually matters:
// whatever TrustBundlePEM emits must parse as a usable trust set that CONTAINS
// this run's injector leaf. A concat that corrupted the PEM framing (a missing
// newline between the last root and the leaf) would still look fine as bytes but
// silently drop the leaf from the pool — and gh would fail to verify the
// injector with no obvious cause.
func TestTrustBundlePEM_ParsesAsPoolWithLeaf(t *testing.T) {
	_, leafPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	leafBlock, _ := pem.Decode(leafPEM)
	if leafBlock == nil {
		t.Fatal("generated leaf is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	bundle := TrustBundlePEM(leafPEM)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("TrustBundlePEM output did not parse as any certificates")
	}
	// The leaf must be in the pool: verifying it against the pool as its own
	// root succeeds only if the concat preserved it intact.
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("injector leaf is not trusted by its own trust bundle: %v", err)
	}
}

// TestTrustBundlePEM_KeepsSystemRootsWhenPresent pins the widening-nothing
// claim's mechanics: when the host has a readable system bundle, the output
// carries strictly more certificates than the leaf alone (the roots the jail
// already trusts via the mounted CA dir), so SSL_CERT_FILE never narrows the
// file-sourced trust of other in-jail TLS clients.
func TestTrustBundlePEM_KeepsSystemRootsWhenPresent(t *testing.T) {
	var hostBundle string
	for _, p := range systemCABundleFiles {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			hostBundle = p
			break
		}
	}
	if hostBundle == "" {
		t.Skip("no system CA bundle on this host; the leaf-only degrade path is covered by the parse test")
	}

	_, leafPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	bundle := TrustBundlePEM(leafPEM)

	if !strings.HasSuffix(string(bundle), string(leafPEM)) {
		t.Error("run leaf is not the final block of the trust bundle")
	}
	if len(bundle) <= len(leafPEM) {
		t.Fatalf("bundle (%d bytes) is not larger than the leaf (%d) despite a system bundle at %s",
			len(bundle), len(leafPEM), hostBundle)
	}

	withRoots := x509.NewCertPool()
	withRoots.AppendCertsFromPEM(bundle)
	leafOnly := x509.NewCertPool()
	leafOnly.AppendCertsFromPEM(leafPEM)
	if len(withRoots.Subjects()) <= len(leafOnly.Subjects()) { //nolint:staticcheck // Subjects is fine for a count assertion here
		t.Error("trust bundle carries no more subjects than the leaf alone — system roots were dropped")
	}
}
