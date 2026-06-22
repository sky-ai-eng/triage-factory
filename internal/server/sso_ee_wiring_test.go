package server_test

// This file is the test-binary counterpart of package main's ee wiring.
//
// The ported SSO suites live in package server (internal) and exercise the
// licensed SSO surface end-to-end: /api/sso/* routes plus the login-path
// enforcement / JIT / verify-test hooks. In production those are mounted by
// installExtensions() during routes() — but only when (a) ee/sso has been
// linked (its init() calls server.RegisterExtension) and (b) the `sso`
// entitlement is active (a verified TF_LICENSE granting it).
//
// A package server (internal) test file can't blank-import ee/sso — ee/sso
// imports internal/server, so the import would cycle. An EXTERNAL test file
// (package server_test) compiled into the same test binary can, and its
// init side effects (extension registration, SSO store-factory registration)
// are process-global, so the internal tests' servers see them.
//
// CI builds carry no baked-in license key, so entitlements default to
// community (SSO off) and installExtensions() would skip the SSO installer,
// leaving every /api/sso/* to fall through to the SPA 404. Registering a
// grant here mirrors a deployment licensed for SSO — the exact condition the
// ported suites assume.

import (
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"

	// init() registers the SSO route/login extension and the postgres +
	// sqlite SSO store factories into the core seams.
	_ "github.com/sky-ai-eng/triage-factory/ee/sso"
)

func init() {
	entitlements.Register(ssoLicensedGrant{})
}

// ssoLicensedGrant licenses exactly the SSO feature for the test binary —
// the minimal grant the ported suites need, nothing else.
type ssoLicensedGrant struct{}

func (ssoLicensedGrant) Has(f entitlements.Feature) bool { return f == entitlements.FeatureSSO }
