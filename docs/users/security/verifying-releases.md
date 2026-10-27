# Verifying releases

Every release artifact is signed keylessly via
[cosign](https://docs.sigstore.dev/) and GitHub OIDC — there's no signing key for
us to manage, rotate, or leak, and no key for you to fetch and pin before
verifying. Each tagged release also carries an SPDX SBOM per archive, and the GHCR
image carries an SBOM + SLSA provenance attestation alongside its signature.

The `--certificate-identity-regexp` below pins the exact workflow file *and* the
tag-push trigger that ran it — not just the repo. A bare repo-name match would also
accept a signature from any other workflow in this repo that happened to hold
`id-token: write` (e.g. one added on a PR branch), which defeats the point of
checking provenance at all.

**Release tarball / checksums** — `checksums.txt` is signed as a blob; verifying
its signature transitively verifies every archive it lists (each line is a sha256
of one archive):

```sh
cosign verify-blob --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/sky-ai-eng/triage-factory/\.github/workflows/release\.yml@refs/tags/.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```

Download `checksums.txt`, `checksums.txt.sig`, and `checksums.txt.pem` from the
release page alongside the archive, then `sha256sum -c checksums.txt` to confirm
the archive you downloaded matches.

**Docker image** — verify by digest or tag:

```sh
cosign verify ghcr.io/sky-ai-eng/triage-factory:vX.Y.Z \
  --certificate-identity-regexp '^https://github\.com/sky-ai-eng/triage-factory/\.github/workflows/docker-publish\.yml@refs/tags/.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Inspect the attached SBOM + provenance attestations with `docker buildx imagetools
inspect ghcr.io/sky-ai-eng/triage-factory:vX.Y.Z`.

The regex above only matches tag-push signatures, scoped to what this section
covers. `docker-publish.yml` also signs on every push to `main` (`:edge`,
`:sha-<short>`) — those are legitimately signed too, but with `@refs/heads/main` in
place of `@refs/tags/...`, so verifying one of those images needs
`--certificate-identity-regexp '^https://github\.com/sky-ai-eng/triage-factory/\.github/workflows/docker-publish\.yml@refs/heads/main$'`
instead.

Each `*.sbom.json` (SPDX) attached to the release is a plain downloadable asset — no
verification tooling needed, just fetch it over HTTPS.

**Air-gapped / no Rekor-Fulcio reachability:** keyless verification needs the signer
to reach GitHub's OIDC issuer at sign time (already true — it runs in Actions) and
the *verifier* to reach the public Rekor transparency log and Fulcio CA at verify
time. If your environment can't reach those, fall back to the plain `sha256sum -c
checksums.txt` check against a `checksums.txt` you fetched over authenticated HTTPS
from the GitHub release page — that gives you integrity (the file wasn't
corrupted/tampered in transit) without the provenance guarantee (that it was GitHub
Actions, specifically, that produced it).
