# Contributing

Start with [CONTRIBUTING.md](../../CONTRIBUTING.md) at the repository root:
how to propose a change, the pull request process, and the CLA. Build, lint
and test commands are in [CLAUDE.md](../../CLAUDE.md).

- [Contributor License Agreement](CLA.md)
- [Running multi mode by hand](multi-mode-by-hand.md): the compose stack
  without `triagefactory deploy`, including a build of your checkout
- [Building an Enterprise Edition feature](ee-features.md): where `ee/` code
  goes, the seams it registers into, and the rules it follows
- [Sandbox egress lanes](sandbox-egress.md): how a sandboxed run reaches the
  network, and how to support a new package ecosystem
- [Regenerating the seccomp profile](seccomp-profile.md): finding and
  validating a syscall the executor needs
- Benchmarks: [sandbox capacity](benchmarks/sandbox-bench.md),
  [polling at scale](benchmarks/poll-bench.md),
  [checkpoint freeze](benchmarks/checkpoint-freeze.md),
  [gVisor CPU cost](benchmarks/gvisor-cpu-tax.md)

Design specs, written for coding agents, are in [`specs/`](../../specs/) at
the repository root.
