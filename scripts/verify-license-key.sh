#!/usr/bin/env bash
#
# Verify the Enterprise PUBLIC key that gets baked into release builds.
#
# TF_LICENSE_PUBLIC_KEY (a repo VARIABLE injected into the goreleaser + Docker ldflags)
# must be exactly the tf-license-prod public key below. A malformed value — a copy-paste
# trailing newline, a truncated paste — or the wrong key would base64-fail in Go, leaving
# ee.publicKeyB64 unparseable, so loadPublicKey() returns nil and every enterprise license
# is silently ignored (EE off). This check makes that failure LOUD (a failed release)
# instead of a silently-crippled build. goreleaser is only exercised on a v* tag, so a
# release gate (release.yml) plus periodic checks (license-key-check.yml) are the only CI
# surface that can catch a bad variable.
#
# Env:
#   TF_LICENSE_PUBLIC_KEY   the value that will be baked (from the repo variable)
#   CANONICAL               "true" on the canonical repo. A fork with no key is a valid
#                           community (EE-off) build, so an empty value is allowed there.
#
# Rotating the prod key is deliberate: update EXPECTED here AND the repo variable in the
# same change. Source of truth: `licensectl pubkey --env prod` (tf-internal ops repo).
set -euo pipefail

EXPECTED="MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAExwX8JiPaEIRK4S1IxytI/FbY28LzBhg1F1q5uwLy47IosslwxxzsxUAFx0xpnGPqoGeadQr9Gw4Um2vuksHdhQ=="

KEY="${TF_LICENSE_PUBLIC_KEY:-}"
CANONICAL="${CANONICAL:-true}"

if [ -z "$KEY" ]; then
  if [ "$CANONICAL" = "true" ]; then
    echo "::error::TF_LICENSE_PUBLIC_KEY is empty — release builds would ship with enterprise features DISABLED. Set the repo variable to the tf-license-prod public key." >&2
    exit 1
  fi
  echo "TF_LICENSE_PUBLIC_KEY empty on a fork — community build (EE off). OK."
  exit 0
fi

if [ "$KEY" != "$EXPECTED" ]; then
  echo "::error::TF_LICENSE_PUBLIC_KEY does not match the expected tf-license-prod public key — likely a malformed value (trailing newline / truncated paste) or the wrong key. Builds would silently ship EE-disabled. If the prod key legitimately rotated, update EXPECTED in scripts/verify-license-key.sh AND the repo variable in the same change." >&2
  exit 1
fi

echo "OK: TF_LICENSE_PUBLIC_KEY is present and matches the expected tf-license-prod public key."
