package egressproxy

import (
	"crypto/tls"
	"crypto/x509"
)

// This file holds every tls.Config the package builds. scripts/lint.sh refuses
// a tls.Config literal anywhere else in the package, and InsecureSkipVerify
// anywhere at all, so a change to how the interceptor verifies a registry has
// to be made here, where it is read as one.

// upstreamTLSConfig is the one configuration the interceptor dials a registry
// with: the system roots, Go's default versions and cipher suites, and the
// CONNECT host as the name to verify. Nothing in it comes from Config, so no
// per-run or per-profile setting can weaken it. Each client in the jail used
// to verify its registry itself; now this dial verifies for all of them while
// each one sees a valid certificate, so a mistake here would weaken every
// toolchain at once without any of them noticing.
func upstreamTLSConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, RootCAs: upstreamRoots}
}

// upstreamRoots is nil in production, which means the host's system roots.
// Only this package's tests set it, to stand a local registry behind a test
// CA.
var upstreamRoots *x509.CertPool

// jailTLSConfig serves the jail's TLS session for host: the leaf ca mints for
// host and no other name, and HTTP/1.1 as the only application protocol. A
// client whose server name is anything else is given no certificate, so its
// handshake fails with an unrecognized_name alert; mismatch records why, for
// the denial the failed handshake reports.
func jailTLSConfig(host string, ca *authority, mismatch func(serverName string)) *tls.Config {
	return &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if normalizeHost(hello.ServerName) != host {
				mismatch(hello.ServerName)
				return &tls.Config{}, nil
			}
			leaf, err := ca.leaf(host)
			if err != nil {
				return nil, err
			}
			return &tls.Config{
				Certificates: []tls.Certificate{*leaf},
				NextProtos:   []string{"http/1.1"},
			}, nil
		},
	}
}
