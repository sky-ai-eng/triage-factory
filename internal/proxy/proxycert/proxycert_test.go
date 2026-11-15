package proxycert

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// TestGenerate pins what a proxy's client relies on: the PEM is the
// certificate the proxy serves, it verifies as its own root for the address
// the proxy binds and for loopback, and it names the proxy it belongs to.
func TestGenerate(t *testing.T) {
	cert, certPEM, err := Generate("llm proxy", "10.42.7.1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("the PEM holds no block")
	}
	if len(cert.Certificate) != 1 || string(cert.Certificate[0]) != string(block.Bytes) {
		t.Fatal("the PEM is not the certificate the proxy serves")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "triage-factory llm proxy" {
		t.Errorf("CommonName = %q, want the proxy's name", leaf.Subject.CommonName)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	for _, host := range []string{"10.42.7.1", "127.0.0.1", "localhost"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
			t.Errorf("a client trusting the PEM cannot verify %s: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "10.42.8.1", Roots: roots}); err == nil {
		t.Error("the certificate verifies for an address it was not minted for")
	}
}
