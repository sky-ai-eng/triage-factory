// Package cabundle builds the trust files TF hands an agent's TLS clients:
// the host's system roots followed by the certificates a run adds of its own.
package cabundle

import "os"

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

// WithSystemRoots returns the host's system CA bundle followed by certs, each
// a PEM block or blocks. Nil entries are skipped.
//
// # Why the public roots are in here
//
// The variables that point a client at this file (SSL_CERT_FILE above all)
// are process-global and REPLACE the client's CA file rather than adding to
// it, for everything that honors them, not just the client a run's own
// certificate is for. A file holding only that certificate would leave every
// other TLS client relying on whatever fallback it has, if any. Concatenating
// makes the trust set explicit in one file.
//
// # Why this widens nothing
//
// The roots are the ones the host already trusts, and in a jail the same
// roots are mounted as the default CA directory. Trust is also not
// reachability: what a jail may connect to is its egress policy, and no trust
// file moves that boundary. A CA bundle is public certificates by
// construction, so nothing secret is in it.
//
// A missing or unreadable system bundle degrades to certs alone rather than
// failing the run.
func WithSystemRoots(certs ...[]byte) []byte {
	var out []byte
	for _, path := range systemCABundleFiles {
		roots, err := os.ReadFile(path)
		if err != nil || len(roots) == 0 {
			continue
		}
		out = appendPEM(out, roots)
		break
	}
	for _, c := range certs {
		out = appendPEM(out, c)
	}
	return out
}

// appendPEM appends block to out, with a newline between them if out does not
// end in one, so the concatenation never runs two PEM blocks together.
func appendPEM(out, block []byte) []byte {
	if len(block) == 0 {
		return out
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, block...)
}
