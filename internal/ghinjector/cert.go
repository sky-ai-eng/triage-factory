package ghinjector

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// GenerateCert mints a per-run self-signed TLS certificate for the injector to
// serve, valid for the given host (the sandbox-reachable veth IP the injector
// binds on) plus loopback. It returns the tls.Certificate (private key held
// only here, in the capless sidecar) and the PEM-encoded PUBLIC certificate —
// the latter is what TrustBundlePEM folds into the trust file bind-mounted
// read-only into the jail, so the agent's gh (whose SSL_CERT_FILE points there)
// trusts this one endpoint. No CA, and no rootfs trust-store edit.
//
// The certificate carries host as an IP SAN (gh forces https to GH_HOST and Go
// verifies the connection host against the SANs), so a bare "host:port" GH_HOST
// pointed at host validates cleanly with no TLS interception anywhere.
func GenerateCert(host string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("ghinjector: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("ghinjector: mint serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "triage-factory gh injector"},
		NotBefore:             now.Add(-time.Hour), // small backdate for clock skew
		NotAfter:              now.Add(24 * time.Hour * 400),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// SAN: the bind host as an IP (the veth IP the sandbox reaches) or a DNS
	// name (test loopback via hostname), plus loopback IPs so a local test that
	// dials 127.0.0.1 verifies too.
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	tmpl.DNSNames = append(tmpl.DNSNames, "localhost")

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("ghinjector: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("ghinjector: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("ghinjector: assemble keypair: %w", err)
	}
	return tlsCert, certPEM, nil
}

// systemCABundleFiles are the conventional single-file CA bundles on Linux, in
// the same order Go's own crypto/x509 probes them (root_linux.go's certFiles).
// Mirrored rather than imported because Go exports neither the list nor a way
// to re-serialize x509.SystemCertPool back to PEM.
var systemCABundleFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian/Ubuntu/Gentoo/Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora/RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS/RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine
}

// TrustBundlePEM builds the trust file the sandboxed gh gets via SSL_CERT_FILE:
// the host's system CA bundle followed by this run's injector leaf.
//
// # Why the public roots are in here
//
// SSL_CERT_FILE is process-global and REPLACES the CA file list for everything
// in the jail that honors it (Go's crypto/x509, OpenSSL) — not just gh. A
// leaf-only file would therefore leave every other in-jail TLS client relying
// on the CA *directory* fallback (SSL_CERT_DIR, unset, so the built-in default
// dirs) to still see the real roots. That fallback does hold today — the host's
// cert dir is bind-mounted at /etc/ssl/certs precisely so outbound TLS verifies
// — but resting package-manager TLS on an unstated interaction between two
// env-var lookups is a trap for whoever edits either side next. Concatenating
// makes the trust set explicit in one file.
//
// # Why this widens nothing
//
// The jail ALREADY trusts these roots: the same host cert dir is mounted into
// every sandbox, gh channel or not, and it is the default CApath. So this adds
// no CA that wasn't already trusted here — it changes where the set is read
// from, not what is in it. Trust is also not reachability: what the agent may
// connect to is the egress proxy's fail-closed allowlist (which deliberately
// excludes GitHub, forcing this channel through the injector), and no trust
// file moves that boundary. A CA bundle is public certs by construction, so
// nothing secret crosses either.
//
// A missing/unreadable system bundle degrades to leaf-only (the pre-existing
// shape, still correct via the CApath fallback) rather than failing the run.
func TrustBundlePEM(leafPEM []byte) []byte {
	for _, path := range systemCABundleFiles {
		roots, err := os.ReadFile(path)
		if err != nil || len(roots) == 0 {
			continue
		}
		out := make([]byte, 0, len(roots)+len(leafPEM)+1)
		out = append(out, roots...)
		if roots[len(roots)-1] != '\n' {
			out = append(out, '\n')
		}
		return append(out, leafPEM...)
	}
	return leafPEM
}
