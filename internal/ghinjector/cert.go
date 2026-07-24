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
	"time"
)

// GenerateCert mints a per-run self-signed TLS certificate for the injector to
// serve, valid for the given host (the sandbox-reachable veth IP the injector
// binds on) plus loopback. It returns the tls.Certificate (private key held
// only here, in the capless sidecar) and the PEM-encoded PUBLIC certificate —
// the latter is what gets bind-mounted read-only into the jail so the agent's
// gh, whose SSL_CERT_FILE points at it, trusts this one endpoint. No CA, no
// rootfs trust-store edit: gh (a Go program) honors SSL_CERT_FILE as its sole
// root set, so a single self-signed leaf is enough.
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
