# Install (local mode)

Local mode runs on macOS, Linux and Windows (amd64 and arm64). There are a few
ways to download the software and a number of possible configuration flags, but
I highly recommend ignoring all of that, using Homebrew to download it on macOS
or Linux, and running it without any flags. On Windows, use the
[release zip](#windows).

```bash
brew tap sky-ai-eng/tap
brew install triagefactory
triagefactory
```

## Prerequisites

- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and Node.js 18
  or later. Triage Factory installs the Claude Code SDK that runs agents the
  first time it needs it, using `npm`.
- `git`.
- On Windows, [Git for Windows](https://git-scm.com/download/win). Claude Code
  runs every agent shell command in Git Bash, which ships with it. Without it,
  the agent runtime refuses to start with
  `the agent runtime needs Git Bash, which ships with Git for Windows`. Triage
  Factory finds Git Bash from where `git` is installed. If yours is somewhere
  else, set `CLAUDE_CODE_GIT_BASH_PATH` to the full path of its `bash.exe`. A
  path that doesn't exist is an error, not ignored.

## First launch

The binary opens the app in your browser, and setup walks you through
connecting GitHub, then optionally Jira or Linear. Either tracker can be added
later in Settings. To put `triagefactory` on your `PATH` when you didn't use
Homebrew, see [Running](running.md#cli-subcommands).

---

## Windows

Windows is supported for local mode only. Install the
[prerequisites](#prerequisites) first.

1. Download `triagefactory_<version>_windows_amd64.zip` (or `windows_arm64`)
   from the
   [latest release](https://github.com/sky-ai-eng/triage-factory/releases/latest)
   and extract it.
2. The binary is not code-signed, so SmartScreen may say "Windows protected your
   PC". Check the download against the release's checksum and signature first
   ([Verifying releases](../security/verifying-releases.md)). Then choose **More
   info**, then **Run anyway**. Or clear the download mark from PowerShell
   before the first run:

   ```powershell
   Unblock-File .\triagefactory.exe
   ```

3. Run it. The browser opens as on other systems:

   ```powershell
   .\triagefactory.exe
   ```

To run `triagefactory` from any terminal, install it:

```powershell
.\triagefactory.exe install
```

This copies the binary to
`%LOCALAPPDATA%\Programs\triagefactory\triagefactory.exe`. If that folder is not
on your `PATH`, install prints a PowerShell line that adds it for your user. Run
it and open a new terminal. Unlike macOS and Linux, the install is a copy, so
re-run `triagefactory install` after upgrading to refresh it. `--dest <path>`
picks another location, and `--force` replaces a file there that Triage Factory
didn't install.

`triagefactory uninstall` deletes your local data and the copy at the default
path above. A copy you installed with `--dest` is not found: delete it by hand.
When you run uninstall from the installed copy, the file is renamed to
`triagefactory.exe.old` and Windows is asked to delete it at the next restart.
Windows accepts that request only from an administrator terminal, so from a
normal terminal the output says it could not be scheduled: delete the `.old`
file by hand after the command exits. A `triagefactory.exe` you ran from
somewhere else stays.

### Long paths

Agent checkouts sit several folders deep, and Windows limits paths to 260
characters unless long paths are on. Triage Factory sets `core.longpaths` for
the Git commands it runs, which covers Git. Other tools can still hit the limit,
so turn on Win32 long paths once, from an elevated PowerShell:

```powershell
New-ItemProperty -Path HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem `
  -Name LongPathsEnabled -Value 1 -PropertyType DWord -Force
```

The group policy **Enable Win32 long paths** does the same. Sign out and back in
for it to apply.

Claude Code also limits the length of the folder name it keeps a run's
conversation in. If your temp folder path is very long, a stopped run can't
be resumed, and Triage Factory logs
`claude project dir name too long to locate`. Point `TEMP`
at a shorter path to avoid it.

---

## Alternate installation methods

### Release tarball

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

### Build from source

Requires Go 1.23+, Node.js 22.13+ (run `corepack enable` once to get the pinned
pnpm), and the [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code).

```bash
git clone https://github.com/sky-ai-eng/triage-factory.git
cd triage-factory
cd frontend && pnpm install && pnpm run build && cd ..
go build -o ./triagefactory .
./triagefactory
```

On Windows, run these in Git Bash and build to `./triagefactory.exe`.
