# Installation

Triage Factory runs as a single binary on your machine.

## Homebrew (macOS/Linux, recommended)

```bash
brew tap sky-ai-eng/tap
brew install triagefactory
triagefactory
```

## Direct download

Grab the tarball for your platform from the [latest release](https://github.com/sky-ai-eng/triage-factory/releases/latest).

Asset names follow:

`triagefactory_<version>_<platform>.tar.gz`

Supported `<platform>` values:

- `darwin_arm64` (Apple Silicon Mac)
- `darwin_amd64` (Intel Mac)
- `linux_amd64`
- `linux_arm64`

Example shell download:

```bash
VERSION=0.1.0
PLATFORM=darwin_arm64

curl -L -o triagefactory.tar.gz \
  "https://github.com/sky-ai-eng/triage-factory/releases/download/v${VERSION}/triagefactory_${VERSION}_${PLATFORM}.tar.gz"
tar xzf triagefactory.tar.gz
./triagefactory
```

On macOS, browser-downloaded binaries may be quarantined. If you see "cannot be opened because the developer cannot be verified", run:

```bash
xattr -d com.apple.quarantine ./triagefactory
```

(`brew install` avoids this by stripping quarantine attributes during install.)

## Build from source

Requirements:

- Go 1.23+
- Node.js 22+ (the frontend uses pnpm, which requires Node 22.13+; run `corepack enable` once to provision the pinned version)
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code)

Build and run:

```bash
git clone https://github.com/sky-ai-eng/triage-factory.git
cd triage-factory

cd frontend && pnpm install && pnpm run build && cd ..
go build -o ./triagefactory .
./triagefactory
```

## First launch

On first launch, setup walks you through connecting GitHub and/or Jira.
