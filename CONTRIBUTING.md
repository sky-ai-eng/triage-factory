# Contributing to Triage Factory

Thanks for contributing.

## Before You Start

By contributing to this repository, you agree that your contributions may be
licensed under the repository's [Triage Factory License 1.0](LICENSE) — which
covers the whole repo, including the [`ee/`](ee/) subtree — as set out in the
contributor license agreement in [`docs/CLA.md`](docs/CLA.md), which all
contributions must be covered by.

Triage Factory follows an open-core model: the repository is source-available,
and the Enterprise Edition (`ee/`) is offered commercially. Under the CLA you
grant the maintainer broad rights to use, relicense, and distribute your
contributions — including as part of paid Enterprise features provided to
customers under separate commercial terms (such as an order form or
subscription agreement). You keep copyright to your work; contributing does
not create any claim to revenue from the project or any say over how the
maintainer licenses or sells it.

Small fixes to `ee/` (bugs, security, tests, docs) are welcome. For larger
`ee/` features, please open an issue to discuss first.

## Contributor License Agreement

This project uses an individual Contributor License Agreement so contributors
keep copyright to their work while granting the maintainer broad rights to use,
relicense, and distribute contributions as part of the project.

For pull requests submitted through GitHub, the expected workflow is:

1. Open a pull request.
2. If prompted by CLA Assistant, review [`docs/CLA.md`](docs/CLA.md) and sign the agreement in the
   PR flow.
3. Wait for the CLA status check to pass before requesting review or merge.

If you are contributing on behalf of an employer or client, make sure you have
permission to submit the work under the terms in [`docs/CLA.md`](docs/CLA.md).

## Pull Requests

Keep pull requests focused and easy to review. Opening a PR loads our
[pull request template](.github/pull_request_template.md) — please fill in each
section rather than deleting it:

- **Problem** — why the change is needed, for a reviewer who hasn't read the ticket
- **Change** — what you did, plus anything intentionally left out of scope
- **Scope / verification** — the commands you ran and what passed (baseline:
  `./scripts/lint.sh` and `go test ./...`), and what new tests cover
- **Line breakdown** — run `./scripts/pr-lines.sh` and paste its table; it
  counts the same diff GitHub shows, split into code, tests, docs and comments

Use a conventional-commit PR title (`type(scope): summary`) and keep the
matching footer line — `Resolves TFAC-NNN` for ticketed work, or `Unticketed`.
The template documents both.

## Code of Conduct

Be respectful, constructive, and professional in issues and pull requests.
