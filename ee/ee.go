// Package ee wires the commercially-licensed Enterprise Edition into the
// binary. It is the boundary the ee/LICENSE governs, and the only ee/
// package that package main imports for enterprise wiring. Core
// (internal/*, cmd/*) never imports ee — dependencies point inward.
//
// As the SSO extraction lands, this package grows blank-import side
// effects (ee/sso registering its store factories, route installers, and
// login hooks into the core seams). Install() handles the licensing half:
// verify the token, register the entitlements checker.
package ee

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/sky-ai-eng/triage-factory/ee/license"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// publicKeyB64 is the base64 (std encoding) Ed25519 PUBLIC key license
// tokens are verified against. Only the public half is baked in; the
// private signing key never ships and lives with the licensor's
// `triagefactory license issue` tooling. Empty in source — release builds
// inject the real key via:
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

func loadPublicKey() ed25519.PublicKey {
	if publicKeyB64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// grant adapts verified license claims to the entitlements.Checker the
// core seam consults. Fails closed: if the clock advances past exp after
// load, Has answers false for every feature.
type grant struct{ claims license.Claims }

func (g grant) Has(f entitlements.Feature) bool {
	return g.claims.Valid(time.Now()) && g.claims.Has(string(f))
}
