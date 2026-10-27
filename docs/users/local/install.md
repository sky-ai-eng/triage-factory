# Install (local mode)

Local mode runs as a single binary on your machine. To deploy Triage Factory for
an organization, see [self-hosting](../self-hosting/install.md) instead.

## Homebrew (macOS and Linux, recommended)

```bash
brew tap sky-ai-eng/tap
brew install triagefactory
triagefactory
```

## Release tarball

Download `triagefactory_<version>_<platform>.tar.gz` from the
[latest release](https://github.com/sky-ai-eng/triage-factory/releases/latest),
where `<platform>` is `darwin_arm64`, `darwin_amd64`, `linux_amd64` or
`linux_arm64`:

```bash
VERSION=0.1.0
PLATFORM=darwin_arm64

curl -L -o triagefactory.tar.gz \
  "https://github.com/sky-ai-eng/triage-factory/releases/download/v${VERSION}/triagefactory_${VERSION}_${PLATFORM}.tar.gz"
tar xzf triagefactory.tar.gz
./triagefactory
```

If macOS says the binary "cannot be opened because the developer cannot be
verified", remove the quarantine attribute (Homebrew does this for you):

```bash
xattr -d com.apple.quarantine ./triagefactory
```

To verify a release's signature first, see
[Verifying releases](../security/verifying-releases.md).

## Build from source

Requires Go 1.23+, Node.js 22.13+ (run `corepack enable` once to get the pinned
pnpm), and the [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code).

```bash
git clone https://github.com/sky-ai-eng/triage-factory.git
cd triage-factory
cd frontend && pnpm install && pnpm run build && cd ..
go build -o ./triagefactory .
./triagefactory
```

## First launch

The binary opens the app in your browser, and setup walks you through
connecting GitHub, then optionally Jira or Linear. Either tracker can be added
later in Settings. To put `triagefactory` on your `PATH` when you didn't use
Homebrew, see [Running](running.md#cli-subcommands).
