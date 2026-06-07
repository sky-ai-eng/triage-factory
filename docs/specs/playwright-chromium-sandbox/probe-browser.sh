#!/bin/sh
# Minimal local spike for docs/specs/playwright-chromium-sandbox (§5/§8).
#
# Builds a "browser profile" alpine rootfs (base + chromium + fonts +
# playwright) exactly as production would (apk into a root, skip the
# glibc browser download), wraps it in an OCI bundle mirroring the REAL
# sandbox posture from internal/sandbox/spec.go (UID 10000, zero caps,
# NoNewPrivileges, readonly rootfs, NOFILE 1024 / NPROC 512), inside a
# pre-created netns with lo up (mirroring netns_linux.go:84), and drives
# runsc the production way (--platform=systrap --ignore-cgroups
# --network=sandbox, joining the netns by path).
#
# It answers the §8 acceptance questions by triangulating over ONE rootfs.
# Renders are split from networking so the seccomp question has no server
# confound (chromium forks its zygote/renderers via clone even for file://):
#
#   run1 none-file       control — does musl chromium render under gVisor
#                        at all (swiftshader, fonts, --no-sandbox)?
#   run2 prod-file       does our CURRENT profile (no clone/clone3,
#                        extracted verbatim from syscalls.go) block it?
#   run3 plusclone-file  does the §5.4 one-line fix (clone/clone3) unblock it?
#   run4 loopback        §3 premise: python http server on 127.0.0.1 +
#                        raw chromium over http + pinned playwright (CDP skew).
#
# Loopback stays in-netns (README §3): zero egress, no veth/proxy/NAT — lo
# is all run4 needs. The repo is bind-mounted at /src; outputs (rootfs,
# screenshots, logs) land under the gitignored _scratch/ so nothing is
# committed. A reproducible validation artifact, not product code.
#
# Reproduce (host needs docker + runsc on PATH; see README §8):
#   docker run --rm --privileged \
#     -v "$(git rev-parse --show-toplevel)":/src \
#     -v /usr/local/bin/runsc:/usr/local/bin/runsc:ro \
#     -w /src alpine:3.20 \
#     sh /src/docs/specs/playwright-chromium-sandbox/probe-browser.sh
set -eu

ALPINE_V=3.20
# Alpine 3.20 ships Chromium 131; pin playwright to its 131-era release
# (1.49.x bundles Chromium 131) so the apk-chromium + playwright pair has
# no CDP-protocol skew. Override with PWPIN= to test other pairings.
PWPIN="${PWPIN:-1.49.1}"
PROBE_DIR=/src/_scratch/browser-probe
WORK="$PROBE_DIR/work"          # bind-mounted at /work (rw) — screenshots land here
BUNDLE=/bundle
ROOT="$BUNDLE/rootfs"
NETNS=tfprobe
NETNS_PATH="/var/run/netns/$NETNS"

banner() { printf '\n\n######## %s ########\n' "$*"; }

# ---------------------------------------------------------------------------
banner "0. harness tooling (host container)"
apk add --no-cache python3 nodejs npm iproute2 >/dev/null
echo "runsc: $(runsc --version | head -1)"
echo "node:  $(node --version)   playwright pin: $PWPIN"
rm -rf "$WORK"; mkdir -p "$WORK/site"

# The static page the renders capture. Self-contained (inline CSS), so
# file:// needs no extra flags. Real text + a colored box → glyph check.
cat > "$WORK/site/index.html" <<HTML
<!doctype html><html><head><meta charset=utf-8><style>
body{font-family:sans-serif;background:#0b3d91;color:#fff;margin:0;padding:40px}
h1{font-size:44px;margin:0 0 16px} p{font-size:22px}
.box{background:#e91e63;padding:18px 24px;border-radius:12px;display:inline-block;font-size:24px}
</style></head><body>
<h1>Triage Factory browser probe</h1>
<p>The quick brown fox jumps over the lazy dog 0123456789</p>
<div class=box>gVisor + Chromium render OK</div>
</body></html>
HTML
# Production chowns the worktree to the sandbox UID before Wrap; mirror it
# so chromium (uid 10000) can read the page and write screenshots.
chown -R 10000:10000 "$WORK"

# ---------------------------------------------------------------------------
banner "1. pre-create netns with lo up (mirrors netns_linux.go)"
ip netns del "$NETNS" 2>/dev/null || true
ip netns add "$NETNS"
ip -n "$NETNS" link set lo up
echo "netns $NETNS interfaces:"; ip -n "$NETNS" addr show lo | head -3
ls -la "$NETNS_PATH"

# ---------------------------------------------------------------------------
banner "2. build browser-profile rootfs (apk --root)"
rm -rf "$BUNDLE"; mkdir -p "$ROOT/etc/apk/keys"
cp -a /etc/apk/keys/. "$ROOT/etc/apk/keys/"
printf 'https://dl-cdn.alpinelinux.org/alpine/v%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/v%s/community\n' \
  "$ALPINE_V" "$ALPINE_V" > "$ROOT/etc/apk/repositories"
# base subset the probe needs (python3 = in-sandbox loopback server) +
# §5.1 browserExtraPackages (chromium + fonts + freetype + nss).
apk add --root "$ROOT" --initdb --no-cache \
  alpine-baselayout busybox musl libc-utils ca-certificates \
  nodejs npm bash python3 \
  chromium font-noto font-noto-cjk font-noto-emoji freetype nss
echo "--- rootfs size: $(du -sh "$ROOT" 2>/dev/null | cut -f1) ---"

# ---------------------------------------------------------------------------
banner "3. install pinned playwright into rootfs (skip browser download)"
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
npm install -g --prefix "$ROOT/usr" "playwright@$PWPIN" >/dev/null 2>&1 \
  || npm install -g --prefix "$ROOT/usr" "playwright@$PWPIN"
echo "playwright in rootfs: $(python3 -c 'import json;print(json.load(open("'"$ROOT"'/usr/lib/node_modules/playwright/package.json"))["version"])' 2>/dev/null || echo UNKNOWN)"

# ---------------------------------------------------------------------------
# In-sandbox payloads. SINGLE-quoted so the OUTER shell leaves them intact;
# the INNER /bin/sh runs them. $OUT comes from process.env (set per run).
# NO single quotes inside (would close the outer quote). JS uses "double".
# ---------------------------------------------------------------------------
CRFLAGS="--headless=new --no-sandbox --disable-setuid-sandbox --disable-dev-shm-usage --disable-gpu --hide-scrollbars --no-first-run"

SB_FILE='set -x
id; uname -srm
/usr/bin/chromium-browser --version 2>&1 | head -1 || true
/usr/bin/chromium-browser '"$CRFLAGS"' --user-data-dir=/tmp/cr --window-size=900,520 \
  --screenshot=/work/$OUT file:///work/site/index.html > /tmp/cr.log 2>&1
echo "[chromium rc=$?]"; tail -8 /tmp/cr.log
ls -l /work/$OUT 2>&1 || echo NO_PNG'

SB_LOOP='set -x
id
python3 -m http.server 8080 --bind 127.0.0.1 --directory /work/site > /tmp/srv.log 2>&1 &
sleep 2
busybox wget -qO- http://127.0.0.1:8080 > /tmp/idx 2>/tmp/wget.log
echo "[loopback wget rc=$? bytes=$(wc -c < /tmp/idx)]"; head -2 /tmp/wget.log 2>/dev/null || true
/usr/bin/chromium-browser '"$CRFLAGS"' --user-data-dir=/tmp/cr2 --window-size=900,520 \
  --screenshot=/work/shot-loopback-raw.png http://127.0.0.1:8080 > /tmp/cr.log 2>&1
echo "[chromium-loopback rc=$?]"; tail -6 /tmp/cr.log
ls -l /work/shot-loopback-raw.png 2>&1 || echo NO_LOOPBACK_PNG
cat > /tmp/pw.js <<JS
const { chromium } = require("playwright");
(async () => {
  const b = await chromium.launch({ executablePath: "/usr/bin/chromium-browser",
    args: ["--no-sandbox","--disable-setuid-sandbox","--disable-dev-shm-usage","--disable-gpu"] });
  const v = b.version();
  const p = await b.newPage({ viewport: { width: 900, height: 520 } });
  await p.goto("http://127.0.0.1:8080", { waitUntil: "load" });
  await p.screenshot({ path: "/work/shot-playwright.png" });
  await b.close();
  console.log("PLAYWRIGHT_OK chromium_version=" + v);
})().catch(e => { console.error("PLAYWRIGHT_ERR", e && e.message ? e.message : e); process.exit(1); });
JS
node /tmp/pw.js > /tmp/pw.log 2>&1
echo "[playwright rc=$?]"; tail -22 /tmp/pw.log
ls -l /work/shot-playwright.png 2>&1 || echo NO_PW_PNG'

# ---------------------------------------------------------------------------
# generate config.json (runsc spec scaffold -> mutate to real posture +
# join the pre-created netns) and run.
# ---------------------------------------------------------------------------
run_sandbox() {
  label="$1"; seccomp_mode="$2"; sbscript="$3"; outname="$4"
  rm -f "$BUNDLE/config.json"               # runsc spec refuses to overwrite
  ( cd "$BUNDLE" && runsc spec )
  SECCOMP_MODE="$seccomp_mode" SB="$sbscript" OUTNAME="$outname" NETNS_PATH="$NETNS_PATH" \
    python3 - "$BUNDLE/config.json" <<'PY'
import json, os, re, sys
cfgp = sys.argv[1]
mode = os.environ["SECCOMP_MODE"]
spec = json.load(open(cfgp))
p = spec["process"]
p["terminal"] = False
p["args"] = ["/bin/sh", "-c", os.environ["SB"]]
p["user"] = {"uid": 10000, "gid": 10000}
p["cwd"] = "/work"
p["noNewPrivileges"] = True
p["env"] = [
    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "HOME=/work", "TMPDIR=/tmp", "TERM=xterm",
    "NODE_PATH=/usr/lib/node_modules",
    "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1",
    "PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser",
    "OUT=" + os.environ["OUTNAME"],
]
p["capabilities"] = {k: [] for k in ("bounding", "effective", "permitted", "inheritable", "ambient")}
p["rlimits"] = [
    {"type": "RLIMIT_NOFILE", "hard": 1024, "soft": 1024},
    {"type": "RLIMIT_NPROC", "hard": 512, "soft": 512},
]
spec["root"] = {"path": "rootfs", "readonly": True}
spec["hostname"] = "tf-sandbox"
spec.setdefault("mounts", []).append({
    "destination": "/work", "type": "bind",
    "source": "/src/_scratch/browser-probe/work", "options": ["rbind", "rw"],
})
# join the pre-created netns (lo up), like production passes netnsPath.
for ns in spec["linux"]["namespaces"]:
    if ns.get("type") == "network":
        ns["path"] = os.environ["NETNS_PATH"]
if mode == "none":
    spec["linux"].pop("seccomp", None)
else:
    src = open("/src/internal/sandbox/syscalls.go").read()
    blk = src.split("defaultAllowedSyscalls = []string{", 1)[1].split("\n}", 1)[0]
    names = re.findall(r'"([^"]+)"', blk)
    if mode == "plusclone":
        names += ["clone", "clone3"]
    spec["linux"]["seccomp"] = {
        "defaultAction": "SCMP_ACT_ERRNO",
        "architectures": ["SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_AARCH64"],
        "syscalls": [{"names": sorted(set(names)), "action": "SCMP_ACT_ALLOW"}],
    }
json.dump(spec, open(cfgp, "w"), indent=2)
n = len(spec["linux"].get("seccomp", {}).get("syscalls", [{}])[0]["names"]) if spec["linux"].get("seccomp") else 0
print("config: seccomp=%s (%d allowed, clone=%s) netns=joined" % (
    mode, n, "YES" if mode == "plusclone" else ("n/a" if mode == "none" else "no")))
PY
  banner "RUN [$label]  seccomp=$seccomp_mode"
  set +e
  timeout 150 runsc --platform=systrap --ignore-cgroups --network=sandbox \
    run --bundle "$BUNDLE" "tfbrowser-$label"
  rc=$?
  set -e
  runsc --ignore-cgroups delete --force "tfbrowser-$label" 2>/dev/null || true
  echo "==== EXIT [$label] rc=$rc ===="
}

run_sandbox none-file      none      "$SB_FILE" shot-none-file.png
run_sandbox prod-file      prod      "$SB_FILE" shot-prod-file.png
run_sandbox plusclone-file plusclone "$SB_FILE" shot-plusclone-file.png
run_sandbox loopback       plusclone "$SB_LOOP" shot-loopback-raw.png

# ---------------------------------------------------------------------------
banner "ACCEPTANCE SUMMARY"
for f in shot-none-file.png shot-prod-file.png shot-plusclone-file.png shot-loopback-raw.png shot-playwright.png; do
  if [ -s "$WORK/$f" ]; then echo "PASS  $f  $(wc -c < "$WORK/$f") bytes"
  else echo "FAIL  $f  (missing/empty)"; fi
done
ip netns del "$NETNS" 2>/dev/null || true
chown -R "$(stat -c '%u:%g' /src)" "$PROBE_DIR" 2>/dev/null || true
echo "screenshots in $PROBE_DIR/work/ — eyeball for real glyphs vs tofu boxes"
