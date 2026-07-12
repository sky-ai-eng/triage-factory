# Security

Reviewer-facing documentation of how Triage Factory isolates tenants and confines
the untrusted agent. These docs span every deployment shape — SaaS (Tier 2),
self-host (Tier 3), and local (Tier 4) — so they live here rather than under
[self-hosting](../self-hosting/).

## Start here

- [Isolation tiers](isolation-tiers.md) — the org-vs-deployment boundary and the tier ladder deployments slot into
- [Security overview](security-overview.md) — the threat model, what privileges TF requires from the host, and what a compromise of each component yields

## Mechanisms

- [Privilege separation: process model](privilege-separation.md) — the cap-broker / orchestrator / sandbox split, and how to verify it against a running deployment
- [Tailored seccomp profile](seccomp-profile.md) — the host-level default-deny allowlist and how to regenerate it
- [Verifying releases](verifying-releases.md) — cosign keyless signatures, SBOM, and SLSA provenance

## Related

- [docs/for-agents/multi-tenant-architecture.html](../for-agents/multi-tenant-architecture.html) — the full multi-tenant design (RLS, Vault, gVisor)
