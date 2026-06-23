// Package ee wires the commercially-licensed Enterprise Edition into the
// binary. It is the boundary the ee/LICENSE governs, and the only ee/
// package that package main imports for enterprise wiring. Core
// (internal/*, cmd/*) never imports ee — dependencies point inward.
//
// package main blank-imports ee/sso, whose init() side effects register the
// SSO store factories, route installers, and login hooks into the core
// seams. This package's Install() handles the licensing half: verify the
// token, register the entitlements checker.
package ee

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/ee/license"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// publicKeyB64 is the standard-base64 of the DER/SPKI (PKIX
// SubjectPublicKeyInfo) encoding of the ECDSA P-256 PUBLIC key license
// tokens are verified against — the conventional public-key encoding a
// signing service emits, so the production key flows straight into ldflags
// with no re-encoding. Only the public half is baked in; the private
// signing key never ships and is held entirely by the licensor's issuing
// service. Empty in source — release builds inject the real key via:
//
//	-ldflags "-X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=<b64>"
var publicKeyB64 = ""

// Install verifies TF_LICENSE (if set) and registers the resulting
// entitlements checker so enterprise features light up. Called once from
// package main. Absent / invalid / expired license, or a build with no
// baked-in public key → no registration, so entitlements.Active() stays
// the community default and every enterprise feature is off. Never fatal:
// a bad license degrades to community, it does not crash the binary.
func Install() {
	token := os.Getenv("TF_LICENSE")
	if token == "" {
		return
	}
	pub := loadPublicKey()
	if pub == nil {
		fmt.Fprintln(os.Stderr, "triagefactory: TF_LICENSE set but this build has no enterprise public key; ignoring")
		return
	}
	claims, err := license.NewVerifier(pub).Verify(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "triagefactory: enterprise license rejected: %v\n", err)
		return
	}
	entitlements.Register(grant{claims})
	// Startup diagnostic → stderr (like the failure paths above), so it doesn't
	// land in the stdout log stream or leak the org/feature list into structured
	// stdout logging.
	fmt.Fprintf(os.Stderr, "Enterprise license: %s (features: %v) valid until %s\n",
		claims.Org, claims.Features, claims.ExpiresAt().Format(time.DateOnly))
}

// loadPublicKey parses the baked-in publicKeyB64 as an ECDSA P-256 public
// key. The encoding is standard-base64 of the DER/SPKI (PKIX) form. Any
// problem — empty key, bad base64, non-PKIX DER, wrong key type, or a curve
// other than P-256 — yields nil, which Install treats as "no enterprise
// key" (community default). Fail-closed: a malformed key never half-loads.
func loadPublicKey() *ecdsa.PublicKey {
	if publicKeyB64 == "" {
		return nil
	}
	der, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return nil
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil
	}
	return ec
}

// grant adapts verified license claims to the entitlements.Checker the
// core seam consults. Fails closed: if the clock advances past exp after
// load, Has answers false for every feature.
type grant struct{ claims license.Claims }

func (g grant) Has(f entitlements.Feature) bool {
	return g.claims.Valid(time.Now()) && g.claims.Has(string(f))
}
