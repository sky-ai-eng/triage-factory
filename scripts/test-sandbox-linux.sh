#!/usr/bin/env bash
# Sandbox smoke test for Linux.
#
# One-shot validation of the internal/sandbox package against real
# runsc on a real Linux box — runs the unit suite, the agentproc
# helpers, and the build-tagged integration tests that exercise the
# full netns + veth + iptables + OCI + runsc pipeline.
#
# Why a wrapper script instead of just `go test -tags integration`:
#   - Self-heals PATH for the sudo+brew interaction (secure_path)
#   - Re-execs under sudo with CAP_NET_ADMIN + CAP_SYS_ADMIN
#   - Validates prereqs (runsc, iptables, ip, go, curl, chroot) with
#     install hints when missing — note: host node is NOT a prereq
#     anymore; nodejs lives in the cached alpine rootfs via apk-add
#   - Inventory of pre-/post-test netns + veth + iptables orphans, so
#     any cleanup regression surfaces immediately
#
# Until we have a CI runner that grants CAP_SYS_ADMIN (gVisor's
# minimum), this script is the de facto CI for the sandbox package.
#
# Usage:
#   ./scripts/test-sandbox-linux.sh
#
# Logs land in /tmp/sky-254-sandbox-test/ (overridable via LOG_DIR).

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Self-heal PATH for brew-installed tools. sudo's secure_path wipes
# PATH back to /usr/sbin:/usr/bin:/sbin:/bin, which hides anything
# installed via linuxbrew or macOS homebrew. Prepend known prefixes
# so check_bin finds go/curl/etc regardless of invocation style.
for brew_prefix in /home/linuxbrew/.linuxbrew /opt/homebrew /usr/local/Homebrew; do
  if [[ -x "$brew_prefix/bin/brew" ]]; then
    export PATH="$brew_prefix/bin:$brew_prefix/sbin:$PATH"
    break
  fi
done

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
blue()   { printf '\033[34m== %s ==\033[0m\n' "$*"; }

LOG_DIR="${LOG_DIR:-/tmp/sky-254-sandbox-test}"
mkdir -p "$LOG_DIR"

# ---------- Platform gate ---------------------------------------------------
if [[ "$(uname -s)" != "Linux" ]]; then
  red "Must run on Linux (got: $(uname -s))"
  exit 1
fi

# ---------- Diagnostic header ----------------------------------------------
blue "Host info"
echo "  kernel:  $(uname -r)"
echo "  distro:  $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || echo unknown)"
echo "  arch:    $(uname -m)"
echo "  log dir: $LOG_DIR"

# ---------- Prereqs --------------------------------------------------------
blue "Prereq check"
missing=0
check_bin() {
  local bin="$1"
  local hint="$2"
  if command -v "$bin" >/dev/null 2>&1; then
    green "  ok    $bin  ($(command -v "$bin"))"
  else
    red   "  MISS  $bin  — install: $hint"
    missing=1
  fi
}
check_bin runsc    "curl -fsSL -o /tmp/runsc https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/runsc && chmod +x /tmp/runsc && sudo mv /tmp/runsc /usr/local/bin/runsc"
check_bin ip       "apt-get install -y iproute2"
check_bin iptables "apt-get install -y iptables"
check_bin go       "https://go.dev/dl/"
check_bin curl     "apt-get install -y curl   # used by alpine rootfs cache download"
check_bin chroot   "apt-get install -y coreutils   # rootfs toolchain build uses chroot+apk"

if (( missing != 0 )); then
  red "Aborting — install the missing tools above and re-run."
  exit 1
fi

if command -v runsc >/dev/null 2>&1; then
  echo "  runsc version: $(runsc --version 2>&1 | head -1)"
fi
if command -v iptables >/dev/null 2>&1; then
  echo "  iptables backend: $(iptables --version 2>&1)"
  yellow "  note: nft-backend iptables can behave subtly differently from legacy; if MASQUERADE rules don't take, try update-alternatives --set iptables /usr/sbin/iptables-legacy"
fi

# ---------- Sudo re-exec ---------------------------------------------------
if [[ "${EUID}" -ne 0 ]]; then
  yellow "Re-execing under sudo (need CAP_NET_ADMIN + CAP_SYS_ADMIN)"
  exec sudo -E "$0" "$@"
fi

# ---------- Sysctl ----------------------------------------------------------
blue "Sysctl prep"
fwd=$(cat /proc/sys/net/ipv4/ip_forward)
echo "  net.ipv4.ip_forward = $fwd"
if [[ "$fwd" -eq 0 ]]; then
  yellow "  enabling ip_forward (sandbox does this too; pre-enabling makes any later errors clearer)"
  echo 1 > /proc/sys/net/ipv4/ip_forward
fi

# ---------- Pre-test orphan inventory ---------------------------------------
blue "Pre-test orphan inventory"
count_netns_orphans() {
  if [[ ! -d /var/run/netns ]]; then echo 0; return; fi
  local n
  n=$(find /var/run/netns -maxdepth 1 -name 'tf-*' 2>/dev/null | wc -l | tr -d ' ')
  echo "${n:-0}"
}
count_veth_orphans() {
  ip link show 2>/dev/null | grep -cE '(vh|vs)-[0-9a-f]+' || true
}
pre_netns=$(count_netns_orphans)
pre_veth=$(count_veth_orphans)
echo "  tf-* netns orphans:  $pre_netns"
echo "  vh-/vs- veth orphans: $pre_veth"
if (( pre_netns > 0 )); then
  ls /var/run/netns/ | grep '^tf-' | sed 's/^/    /'
  yellow "  the reaper will attempt to clean these on first sandbox.ReapOrphans call"
fi

# ---------- Stage 1: unit tests (always-runnable) --------------------------
blue "Stage 1: unit tests (./internal/sandbox)"
unit_log="$LOG_DIR/unit.log"
if go test -count=1 ./internal/sandbox/... > "$unit_log" 2>&1; then
  green "  unit tests PASS"
else
  red   "  unit tests FAIL — full log: $unit_log"
  echo "---- last 80 lines ----"
  tail -80 "$unit_log"
  exit 1
fi

# ---------- Stage 2: cross-package build sanity ----------------------------
# `go build ./...` without -o compiles every package (typecheck +
# link) but writes no binaries to disk. -o <file> ./... would error
# with "cannot write multiple packages to non-directory" because
# ./... matches more than one main package.
blue "Stage 2: full build"
build_log="$LOG_DIR/build.log"
if go build ./... > "$build_log" 2>&1; then
  green "  build clean"
else
  red   "  build FAIL — full log: $build_log"
  echo "---- last 60 lines ----"
  tail -60 "$build_log"
  exit 1
fi

# ---------- Stage 3: agentproc tests (the sandbox-integration helpers) -----
blue "Stage 3: agentproc package tests"
ap_log="$LOG_DIR/agentproc.log"
if go test -count=1 ./internal/agentproc/... > "$ap_log" 2>&1; then
  green "  agentproc tests PASS"
else
  red   "  agentproc tests FAIL — full log: $ap_log"
  echo "---- last 80 lines ----"
  tail -80 "$ap_log"
  exit 1
fi

# ---------- Stage 4: integration suite (the meat) --------------------------
blue "Stage 4: sandbox integration suite (busybox payloads, build tag 'integration')"
integ_log="$LOG_DIR/integration.log"
if go test -count=1 -v -tags integration ./internal/sandbox/... > "$integ_log" 2>&1; then
  green "  integration tests PASS"
else
  red   "  integration tests FAIL — full log: $integ_log"
  echo "---- last 120 lines ----"
  tail -120 "$integ_log"
  # don't exit yet — we still want the post-test orphan report below
  integration_failed=1
fi

# ---------- Stage 4b: relocation integration (sidecar-hosted agenthost) -----
# The exec-verb socket server relocated INTO the credential sidecar: this runs
# the relocated host exactly the way the sidecar does (NewServerWithRuntime over
# a relay runtime on the real /run/tf socket) and drives a real IPCClient
# through it — LookupConversation + a DB verb that relays to the orchestrator's
# RelayServer. Root-gated (the socket lives under /run/tf); needs no runsc, so
# it complements the gVisor connect-mechanism check in Stage 4's
# TestIntegration_AgentHostIPC_RoundTrip.
blue "Stage 4b: agenthost relocation integration (relocated exec-verb socket + relay)"
reloc_log="$LOG_DIR/relocation.log"
if go test -count=1 -v -tags integration -run TestIntegration_RelocatedAgentHost ./cmd/exec/agenthost/... > "$reloc_log" 2>&1; then
  green "  relocation integration PASS"
else
  red   "  relocation integration FAIL — full log: $reloc_log"
  echo "---- last 80 lines ----"
  tail -80 "$reloc_log"
  integration_failed=1
fi

# ---------- Per-test signal extraction --------------------------------------
blue "Per-test signals (from integration log)"
for test in \
  TestIntegration_BootBusyboxPayload \
  TestIntegration_PropertyB_NoCredentialsInEnv \
  TestIntegration_NonRootUID \
  TestIntegration_WorktreeIsolation \
  TestIntegration_CleanupRemovesNetns \
  TestIntegration_ReapOrphans \
  TestIntegration_SidecarEviction_UIDBandVacates \
  TestIntegration_CrossSidecarSocketIsolation \
  TestIntegration_CrossSidecarPtraceDenied \
  TestIntegration_RootfsHasNode \
  TestIntegration_RootfsHasGit \
  TestIntegration_RootfsHasRipgrep \
  TestIntegration_RootfsHasPython \
  TestIntegration_RootfsHasGo; do
  if   grep -q -- "--- PASS: $test" "$integ_log" 2>/dev/null; then green  "  PASS  $test"
  elif grep -q -- "--- SKIP: $test" "$integ_log" 2>/dev/null; then yellow "  SKIP  $test"
  elif grep -q -- "--- FAIL: $test" "$integ_log" 2>/dev/null; then red    "  FAIL  $test"
  else                                                              yellow "  ????  $test  (no marker in log)"
  fi
done

# ---------- Post-test orphan check -----------------------------------------
blue "Post-test orphan check"
post_netns=$(count_netns_orphans)
post_veth=$(count_veth_orphans)
echo "  tf-* netns remaining: $post_netns (pre: $pre_netns)"
echo "  veth remaining:       $post_veth (pre: $pre_veth)"

leak=0
if (( post_netns > pre_netns )); then
  red "  LEAK: $((post_netns - pre_netns)) new tf-* netns left behind"
  find /var/run/netns -maxdepth 1 -name 'tf-*' | sed 's/^/    /'
  leak=1
fi
if (( post_veth > pre_veth )); then
  red "  LEAK: $((post_veth - pre_veth)) new veth pairs left behind"
  ip link show | grep -E '(vh|vs)-[0-9a-f]+' | sed 's/^/    /'
  leak=1
fi
if (( leak == 0 )); then
  green "  no orphan leaks"
fi

# ---------- iptables MASQUERADE rule check ---------------------------------
blue "iptables NAT chain inspection"
nat_rules=$(iptables -t nat -S POSTROUTING 2>/dev/null | grep -c '10\.42\.' || true)
echo "  10.42.x.x MASQUERADE rules in POSTROUTING: $nat_rules"
if (( nat_rules > 0 )); then
  red "  LEAK: stale MASQUERADE rules left behind"
  iptables -t nat -S POSTROUTING | grep '10\.42\.' | sed 's/^/    /'
  leak=1
fi

# ---------- Verdict --------------------------------------------------------
echo
if [[ -n "${integration_failed:-}" || $leak -ne 0 ]]; then
  red "VERDICT: FAIL"
  echo "  - check $integ_log for test failures"
  echo "  - if leaks: run \`ip netns delete <name>\` and \`iptables -t nat -D POSTROUTING ...\` to clean"
  exit 1
else
  green "VERDICT: PASS"
  echo "  Validated:"
  echo "    - boot, non-root UID, fs isolation, Property B env curation"
  echo "    - cleanup on Close, orphan recovery (netns + iptables)"
  echo "  Coverage scope is whatever lives in internal/sandbox/*_test.go +"
  echo "  internal/sandbox/integration_linux_test.go — add new cases there"
  echo "  as the sandbox surface grows (node-in-rootfs, proxy wiring, etc)."
  echo
  echo "  Logs: $LOG_DIR"
fi
