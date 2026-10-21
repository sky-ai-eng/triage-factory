package cabundle

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// selfSignedPEM is a throwaway certificate standing in for a run's own.
func selfSignedPEM(t *testing.T, name string) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestWithSystemRoots_ParsesWithEveryCert is the property that matters:
// whatever WithSystemRoots emits must parse as a trust set containing each
// cert it was given. A concatenation that broke PEM framing (no newline
// between blocks) would still look fine as bytes but silently drop a
// certificate, and the client it was for would fail with no obvious cause.
func TestWithSystemRoots_ParsesWithEveryCert(t *testing.T) {
	a, aPEM := selfSignedPEM(t, "a.example")
	b, bPEM := selfSignedPEM(t, "b.example")
	// No trailing newline on the first, to exercise the separator.
	bundle := WithSystemRoots([]byte(strings.TrimRight(string(aPEM), "\n")), nil, bPEM)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("bundle did not parse as any certificates")
	}
	for _, c := range []*x509.Certificate{a, b} {
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("%s is not trusted by the bundle: %v", c.Subject.CommonName, err)
		}
	}
}

// TestWithSystemRoots_KeepsSystemRootsWhenPresent pins the widening-nothing
// mechanics: with a readable system bundle, the output carries the roots
// first and then the run's certs, so pointing a client at it never narrows
// what that client already trusted.
func TestWithSystemRoots_KeepsSystemRootsWhenPresent(t *testing.T) {
	var hostBundle string
	for _, p := range systemCABundleFiles {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			hostBundle = p
			break
		}
	}
	if hostBundle == "" {
		t.Skip("no system CA bundle on this host; the certs-only path is covered by the parse test")
	}
	_, certPEM := selfSignedPEM(t, "run.example")
	bundle := WithSystemRoots(certPEM)
	if !strings.HasSuffix(string(bundle), string(certPEM)) {
		t.Error("the run's certificate is not the final block of the bundle")
	}
	withRoots := x509.NewCertPool()
	withRoots.AppendCertsFromPEM(bundle)
	certOnly := x509.NewCertPool()
	certOnly.AppendCertsFromPEM(certPEM)
	if len(withRoots.Subjects()) <= len(certOnly.Subjects()) { //nolint:staticcheck // Subjects is fine for a count assertion here
		t.Errorf("bundle carries no more subjects than the run's certificate; the roots at %s were dropped", hostBundle)
	}
}
