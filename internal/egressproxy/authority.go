package egressproxy

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
	"sync"
	"time"
)

// authorityValidity is how long a per-engagement CA, and every leaf it signs,
// stays valid. The jail reads the CA's certificate once, at launch, so this
// has to outlast the engagement; a run that outlasts it gets certificate
// errors on every intercepted host, never a fallback. The length guards
// nothing: the key dies with the sidecar and the next engagement mints a new
// CA, so the certificate is trusted for one engagement however long it is
// valid. A week only keeps a long engagement from running into it.
const authorityValidity = 7 * 24 * time.Hour

// authorityBackdate allows for a jail clock slightly behind the host's.
const authorityBackdate = time.Hour

// authority is one engagement's interception CA. Its private key exists only
// in this process's memory: it is never serialized, so nothing written to the
// sealed bundle, the run tree or a workspace snapshot can carry it. Only the
// certificate leaves, through certPEM, into the jail's trust file.
//
// The certificate is name-constrained to exactly the hosts the tunnel admits
// and excludes every IP address, so a leaf it signs for anything else fails
// verification in every client, even one handed the key.
type authority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// newAuthority mints the CA for hosts, which must already be normalized.
// There is no CA for an empty list: a certificate with no permitted names
// would carry no name constraint at all, and with no host admitted there is
// nothing to intercept.
func newAuthority(hosts []string, now time.Time) (*authority, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Triage Factory egress interception (this run only)"},
		NotBefore:             now.Add(-authorityBackdate),
		NotAfter:              now.Add(authorityValidity),
		KeyUsage:              x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,

		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         append([]string(nil), hosts...),
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IP{0, 0, 0, 0}, Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: parse CA certificate: %w", err)
	}
	return &authority{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves:  make(map[string]*tls.Certificate),
	}, nil
}

// leaf returns the certificate the jail is shown for host, minting it on first
// use. host is the CONNECT host, already checked against the allowlist; the
// leaf names it and nothing else.
func (a *authority) leaf(host string) (*tls.Certificate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.leaves[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    a.cert.NotBefore,
		NotAfter:     a.cert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: sign leaf for %s: %w", host, err)
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	a.leaves[host] = c
	return c, nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("egressproxy: mint certificate serial: %w", err)
	}
	return serial, nil
}
