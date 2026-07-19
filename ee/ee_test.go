package ee

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/ee/license"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// The production public key ships as publicKeyB64's source default, so a bad
// edit would silently produce a build that ignores (or worse, mis-verifies)
// every official license. Pinning exact equality catches a *valid but wrong*
// key too — e.g. a dev key accidentally committed after local debugging.
// Deliberately an independent copy of the literal; rotation is documented on
// publicKeyB64 itself.
func TestShippedPublicKeyDefaultLoads(t *testing.T) {
	const prodKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAExwX8JiPaEIRK4S1IxytI/FbY28LzBhg1F1q5uwLy47IosslwxxzsxUAFx0xpnGPqoGeadQr9Gw4Um2vuksHdhQ=="
	if publicKeyB64 != prodKey {
		t.Fatalf("publicKeyB64 source default is not the tf-license-prod public key — official builds would reject (wrong key) or ignore (empty/corrupt) every official license\n got: %q", publicKeyB64)
	}
	if loadPublicKey() == nil {
		t.Fatal("publicKeyB64 source default does not parse as a P-256 SPKI public key — official builds would ignore TF_LICENSE entirely")
	}
}

func TestLicenseProvider_ValidClaimsGrantEveryOrg(t *testing.T) {
	claims := license.Claims{
		Org:      "acme",
		Features: []string{string(entitlements.FeatureGovernance)},
		Expires:  time.Now().Add(time.Hour).Unix(),
	}
	p := licenseProvider{
		claims: claims,
		ent:    entitlements.New([]entitlements.Feature{entitlements.FeatureGovernance}, nil),
	}
	for _, org := range []string{"orgA", "orgB"} {
		if !p.For(org).Has(entitlements.FeatureGovernance) {
			t.Fatalf("For(%q) should grant governance under a valid, self-host auto-all license", org)
		}
	}
}

// newLicenseProvider mirrors the construction in Install so the tests exercise
// the real seatLimits → New → snapshot wiring rather than a hand-built map.
func newLicenseProvider(claims license.Claims) licenseProvider {
	feats := make([]entitlements.Feature, len(claims.Features))
	for i, f := range claims.Features {
		feats[i] = entitlements.Feature(f)
	}
	return licenseProvider{
		claims:        claims,
		ent:           entitlements.New(feats, nil),
		deploymentEnt: entitlements.New(feats, seatLimits(claims)),
	}
}

func TestLicenseProvider_MaxSeatsSurfacedOnActive(t *testing.T) {
	p := newLicenseProvider(license.Claims{
		Org: "acme", Features: []string{string(entitlements.FeatureGovernance)},
		MaxSeats: 25, Expires: time.Now().Add(time.Hour).Unix(),
	})
	// Deployment-scoped: the cap reads through Active().
	got, ok := p.Active().Limit(entitlements.LimitSeats)
	if !ok || got != 25 {
		t.Fatalf("Active().Limit(LimitSeats) = (%d, %v), want (25, true)", got, ok)
	}
	// Per-org seam deliberately does NOT carry the deployment-wide cap.
	if _, ok := p.For("acme").Limit(entitlements.LimitSeats); ok {
		t.Fatal("For(org).Limit(LimitSeats) should be unset — the seat cap is deployment-scoped, not per-org")
	}
}

func TestLicenseProvider_UncappedWhenNoMaxSeats(t *testing.T) {
	p := newLicenseProvider(license.Claims{
		Org: "acme", Features: []string{string(entitlements.FeatureGovernance)},
		Expires: time.Now().Add(time.Hour).Unix(),
	})
	if v, ok := p.Active().Limit(entitlements.LimitSeats); ok {
		t.Fatalf("a license with no maxSeats must be uncapped; got Limit(LimitSeats) = (%d, true)", v)
	}
}

func TestLicenseProvider_ExpiredDeniesSeatCap(t *testing.T) {
	p := newLicenseProvider(license.Claims{
		Org: "acme", Features: []string{string(entitlements.FeatureGovernance)},
		MaxSeats: 25, Expires: time.Now().Add(-time.Hour).Unix(),
	})
	if v, ok := p.Active().Limit(entitlements.LimitSeats); ok {
		t.Fatalf("an expired license must report the seat cap unset; got (%d, true)", v)
	}
}

func TestLicenseProvider_ExpiredClaimsDenyLive(t *testing.T) {
	claims := license.Claims{
		Org:      "acme",
		Features: []string{string(entitlements.FeatureGovernance)},
		Expires:  time.Now().Add(-time.Hour).Unix(),
	}
	p := licenseProvider{
		claims: claims,
		ent:    entitlements.New([]entitlements.Feature{entitlements.FeatureGovernance}, nil),
	}
	if p.For("any-org").Has(entitlements.FeatureGovernance) {
		t.Fatal("an expired license must deny every feature, live, without re-registering")
	}
}
