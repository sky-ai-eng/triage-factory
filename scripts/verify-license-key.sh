#!/usr/bin/env bash
#
# Verify the Enterprise PUBLIC key that ships as ee.publicKeyB64's SOURCE DEFAULT
# (ee/ee.go). Every build — the published image, a compose build, a plain `go build` —
# bakes this default, so a mangled edit (truncated paste, stray whitespace, wrong key)
# would ship builds that silently ignore every official license (loadPublicKey fails
# closed to nil → EE off). This check makes that failure LOUD, before an expensive
# release/publish build. TestShippedPublicKeyDefaultLoads (ee/ee_test.go) covers the
# same class on every `go test ./...`; this script is the pre-build belt to that
# suspender and additionally pins the default to the expected tf-license-prod key,
# catching a wrong-key paste that still parses.
#
# Rotating the prod key is deliberate: update ee/ee.go's default, the pin in
# TestShippedPublicKeyDefaultLoads, AND EXPECTED here in the same change, then reissue
# customer tokens. Source of truth: `licensectl pubkey --env prod` (tf-internal ops repo).
set -euo pipefail

cd "$(dirname "$0")/.."

EXPECTED="MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAExwX8JiPaEIRK4S1IxytI/FbY28LzBhg1F1q5uwLy47IosslwxxzsxUAFx0xpnGPqoGeadQr9Gw4Um2vuksHdhQ=="

# A fixed-string presence check, deliberately NOT a positional extraction: parsing the
# declaration (sed on `var publicKeyB64 = ...`) would couple this gate to formatting and
# hard-fail a release over an innocent refactor (e.g. regrouping into a var block). The
# quoted literal either appears in the file or it doesn't, wherever the declaration
# lives. The semantic check — the VARIABLE actually holds this value — is the unit
# test's job (it reads the real var, immune to formatting by construction).
if ! grep -qF "\"$EXPECTED\"" ee/ee.go; then
  echo "::error::the expected tf-license-prod public key does not appear as a string literal in ee/ee.go — a mangled edit (truncated paste / stray whitespace) or the wrong key. Builds would silently ship EE-disabled. If the prod key legitimately rotated, update the ee/ee.go default, the TestShippedPublicKeyDefaultLoads pin, AND EXPECTED in scripts/verify-license-key.sh in the same change." >&2
  exit 1
fi

# Belt-and-suspenders: the pinned value must itself parse as a P-256 SPKI public
# key, so EXPECTED can't drift into something Go's loader would reject.
if ! echo "$EXPECTED" | base64 -d 2>/dev/null | openssl pkey -pubin -inform DER -noout 2>/dev/null; then
  echo "::error::the pinned Enterprise public key does not parse as a DER/SPKI public key — both ee/ee.go and EXPECTED in scripts/verify-license-key.sh hold a corrupt value." >&2
  exit 1
fi

echo "OK: the expected tf-license-prod public key is present in ee/ee.go and parses."
