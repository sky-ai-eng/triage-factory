// Package ee wires the commercially-licensed Enterprise Edition into the
// binary. It is the only ee/ package that package main imports for
// enterprise wiring. Core (internal/*, cmd/*) never imports ee —
// dependencies point inward.
//
// package main blank-imports ee/sso, whose init() side effects register the
// SSO store factories, route installers, and login hooks into the core
// seams. This package's Install() handles the licensing half: verify the
// token, register the per-org entitlements provider.
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
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
)

// publicKeyB64 is the standard-base64 of the DER/SPKI (PKIX
// SubjectPublicKeyInfo) encoding of the ECDSA P-256 PUBLIC key license
// tokens are verified against. The production key is the source default so
// every build verifies official license tokens. Only the public half — the
// private signing key is held entirely by the licensor's issuing service,
// so committing it grants nothing beyond the ability to verify.
//
// Builds against a different issuer (a dev signing key, a fork running its
// own) override it at link time:
//
//	-ldflags "-X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=<b64>"
//
// An explicit empty override (`-X ...publicKeyB64=`) yields a build with no
// key at all — the community posture, where TF_LICENSE is ignored outright.
//
// Rotating the key: update this constant and the TestShippedPublicKeyDefaultLoads
// pin in the same change, then reissue customer tokens under the new key. The
// pin is an independent copy on purpose — deriving it from this variable would
// verify the source against itself.
var publicKeyB64 = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAExwX8JiPaEIRK4S1IxytI/FbY28LzBhg1F1q5uwLy47IosslwxxzsxUAFx0xpnGPqoGeadQr9Gw4Um2vuksHdhQ=="

// Install verifies TF_LICENSE (if set) and registers the resulting per-org
// entitlements provider so enterprise features light up. Called once from
// package main. Absent / invalid / expired license, or a build with no
// baked-in public key → no registration, so entitlements.For(orgID) stays
// the Static (everything off) default and every enterprise feature is off.
// Never fatal: a bad license degrades to the Static (everything off)
// default, it does not crash the binary.
func Install() {
	token := secretenv.Get("TF_LICENSE")
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
	feats := make([]entitlements.Feature, len(claims.Features))
	for i, f := range claims.Features {
		feats[i] = entitlements.Feature(f)
	}
	lp := licenseProvider{
		claims: claims,
		// Per-org: features auto-all; deployment-wide Limits (the seat cap) are
		// deliberately NOT surfaced per-org — a per-seat cap is a deployment
		// property, read through Active() below, and the login critical edge that
		// enforces it has no org to scope on anyway.
		ent: entitlements.New(feats, nil),
		// Deployment: the same features plus the committed seat cap. Enforcement
		// reads entitlements.Active().Limit(entitlements.LimitSeats) — core never
		// names a license/EE type to get the number.
		deploymentEnt: entitlements.New(feats, seatLimits(claims)),
	}
	entitlements.RegisterProvider(lp)
	// The deployment-scoped resolver Active() reads: for self-host the
	// deployment features ARE the license features (the same expiry-checked
	// snapshot), so the one provider backs both seams. On a future SaaS build
	// For would swap to a Stripe-per-org resolver while Active stays
	// license-backed — the reason fleet administration resolves here, never
	// through For(orgID).
	entitlements.RegisterDeploymentProvider(lp)
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

// seatLimits builds the deployment-scoped limits map from the license's
// maxSeats claim. MaxSeats <= 0 (absent / uncapped) yields nil, so
// Active().Limit(LimitSeats) reports (0, false) — uncapped — rather than a
// concrete zero cap that would (wrongly) block every login.
func seatLimits(claims license.Claims) map[entitlements.Limit]int {
	if claims.MaxSeats <= 0 {
		return nil
	}
	return map[entitlements.Limit]int{entitlements.LimitSeats: claims.MaxSeats}
}

// licenseProvider adapts verified license claims to the entitlements seams:
// self-host licensing is "auto-all" — one license grants its features to every
// org in the deployment. Fails closed: once the clock passes exp, both For and
// Active return the zero-value Entitlements (denies everything, reports every
// limit unset). The per-org (ent) and deployment (deploymentEnt) snapshots
// carry the same features; only deploymentEnt carries the seat cap, keeping the
// deployment-wide limit off the per-org seam.
type licenseProvider struct {
	claims        license.Claims
	ent           entitlements.Entitlements
	deploymentEnt entitlements.Entitlements
}

func (p licenseProvider) For(string) entitlements.Entitlements {
	if !p.claims.Valid(time.Now()) {
		return entitlements.Entitlements{}
	}
	return p.ent
}

// Active backs entitlements.Active() — the deployment-scoped snapshot. Same
// expiry-checked feature set as For (self-host licensing is deployment-wide),
// plus the committed seat cap (LimitSeats), so a lapsed license both darkens
// the fleet console and drops the cap to uncapped. This is the snapshot seat
// enforcement and the fleet-console gate (is_operator AND Active().Has(
// FeatureFleet)) resolve through, without naming an org.
func (p licenseProvider) Active() entitlements.Entitlements {
	if !p.claims.Valid(time.Now()) {
		return entitlements.Entitlements{}
	}
	return p.deploymentEnt
}
