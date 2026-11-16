// Package proxycert mints the certificate a per-run proxy serves TLS with.
package proxycert

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

// Generate mints a self-signed TLS certificate valid for host (the address the
// proxy binds on and its clients dial) plus loopback. It returns the
// tls.Certificate, whose private key stays with the proxy that serves it, and
// the PEM-encoded public certificate, which is what a client of that one proxy
// is given to trust. The certificate is its own issuer: no CA is minted, and no
// trust store is edited.
//
// name names the proxy in the certificate's CommonName ("triage-factory
// <name>"), which is what a TLS error reports, and how one run's certificates
// are told apart in a bundle that holds several.
//
// host becomes an IP SAN when it parses as an IP, else a DNS SAN, so a client
// dialing "host:port" verifies the connection against it with no TLS
// interception anywhere.
func Generate(name, host string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("proxycert: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("proxycert: mint serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "triage-factory " + name},
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
		return tls.Certificate{}, nil, fmt.Errorf("proxycert: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("proxycert: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("proxycert: assemble keypair: %w", err)
	}
	return tlsCert, certPEM, nil
}
