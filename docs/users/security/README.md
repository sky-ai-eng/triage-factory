# Security

How Triage Factory isolates tenants and confines the agent, for anyone
evaluating it. These pages cover hosted, self-hosted and local deployments.

- [Isolation tiers](isolation-tiers.md): the org and deployment boundaries,
  and which deployment shape fits a trust requirement
- [Security overview](security-overview.md): threat model, what the host must
  grant, and what a compromise of each component yields
- [Privilege separation](privilege-separation.md): the executor's processes
  and how to check them on a running deployment
- [Seccomp profile](seccomp-profile.md): the executor's syscall allowlist
- [Verifying releases](verifying-releases.md): cosign signatures, SBOMs and
  SLSA provenance
