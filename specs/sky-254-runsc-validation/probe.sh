#!/usr/bin/env bash
# v3 probe: production-mode `runsc run` with a real OCI bundle.
#
# This is the actual model the TF spawner will use. Tests:
#   - Filesystem isolation: sandbox sees a separate rootfs (alpine),
#     NOT the host filesystem. Host secrets must be invisible.
#   - Worktree exposure: a specific host dir bind-mounted at /work
#     IS visible inside the sandbox, with the content we put there.
#   - Network: --network=sandbox isolates loopback from the parent,
#     outbound HTTPS still works.
#   - Env curation: only the env we put in process.env is visible.

set -euxo pipefail
exec 2>&1

hr() { printf '\n\n=== %s ===\n' "$*"; }

# ----------------------------------------------------------------------
hr "Phase 1: parent-side state (must remain invisible to sandbox)"
export PARENT_ANTHROPIC_KEY="sk-ant-FAKE-PARENT-SECRET"
export PARENT_PG_PASSWORD="parent-pw-leak-detector"
echo "parent-fs-secret-should-not-leak" > /tmp/parent-secret.txt
chmod 600 /tmp/parent-secret.txt
ls -la /tmp/parent-secret.txt

# Two fake services:
# - 127.0.0.1:5432 — only on loopback, sandbox shouldn't reach via netns
# - 0.0.0.0:5433   — on all interfaces, sandbox MIGHT reach via veth IP
nohup python3 -c "
import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(b'PARENT-LOOPBACK-5432-SHOULD-NOT-LEAK')
    def log_message(self,*a,**k): pass
socketserver.TCPServer(('127.0.0.1', 5432), H).serve_forever()
" > /tmp/fake5432.log 2>&1 &
nohup python3 -c "
import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(b'PARENT-ALLIFACE-5433-MIGHT-LEAK')
    def log_message(self,*a,**k): pass
socketserver.TCPServer(('0.0.0.0', 5433), H).serve_forever()
" > /tmp/fake5433.log 2>&1 &
sleep 2

# Sanity from parent
echo "Parent->127.0.0.1:5432: $(wget -qO- --timeout=2 http://127.0.0.1:5432 || echo failed)"
echo "Parent->127.0.0.1:5433: $(wget -qO- --timeout=2 http://127.0.0.1:5433 || echo failed)"

# Parent eth0 IP — useful for the "via internal network" attack test
PARENT_IP=$(ip -4 addr show eth0 2>/dev/null | awk '/inet /{print $2}' | cut -d/ -f1 | head -1)
echo "Parent eth0 IPv4: ${PARENT_IP:-(none)}"

# ----------------------------------------------------------------------
hr "Phase 2: build OCI bundle (alpine rootfs + custom spec)"
BUNDLE=/tmp/bundle
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/rootfs"

curl -fsSL -o /tmp/alpine.tgz \
  https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.3-x86_64.tar.gz
tar -xzf /tmp/alpine.tgz -C "$BUNDLE/rootfs"
echo "rootfs contents:"
ls "$BUNDLE/rootfs" | head -20

# Set up a "worktree" on the host that we explicitly want the sandbox
# to see. This is the production model: a per-run dir bind-mounted into
# the sandbox, everything else of the host filesystem invisible.
mkdir -p /data/worktree
echo "worktree-content-from-host" > /data/worktree/agent-input.md
echo "another worktree file" > /data/worktree/notes.txt

# Generate the default OCI spec via `runsc spec`, then mutate it. This
# avoids hand-rolling things runsc expects (capabilities, masked paths,
# readonly paths, etc.).
( cd "$BUNDLE" && runsc spec )
echo "Default spec namespaces:"
grep -A3 namespaces "$BUNDLE/config.json"
echo

# Replace process.args + add /work bind mount, leave everything else
# at runsc's defaults.
python3 <<PYEOF
import json
p = "$BUNDLE/config.json"
with open(p) as f:
    spec = json.load(f)
spec["process"]["args"] = ["/bin/sh", "-c",
    "set -x; echo '[S.0] net diagnostics'; ip addr 2>&1 | head -30; "
    "ip route 2>&1; cat /etc/resolv.conf 2>&1; echo; "
    "echo '[S.1] id+uname'; id; uname -a; echo; "
    "echo '[S.3] read host secret (should fail)'; cat /tmp/parent-secret.txt 2>&1; echo; "
    "echo '[S.5] worktree mount'; ls /work 2>&1; cat /work/agent-input.md 2>&1; echo; "
    "echo '[S.6] env'; env | sort; echo; "
    "echo '[S.7] parent 127.0.0.1:5432'; wget -qO- --timeout=3 http://127.0.0.1:5432 2>&1 || echo BLOCKED; echo; "
    "echo '[S.10] outbound HTTPS api.github.com'; wget -qO- --timeout=10 https://api.github.com/zen 2>&1 || echo FAILED"
]
spec["process"]["env"] = [
    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "TERM=xterm",
    "AGENT_CURATED_KEY=sk-ant-CURATED-VISIBLE-IN-SANDBOX",
]
spec["process"]["terminal"] = False
spec["hostname"] = "tf-sandbox"
# Bind-mount the host's worktree dir into /work. This is the only host
# path the sandbox should see beyond its own rootfs.
spec.setdefault("mounts", []).append({
    "destination": "/work",
    "type": "bind",
    "source": "/data/worktree",
    "options": ["rbind", "rw"],
})
with open(p, "w") as f:
    json.dump(spec, f, indent=2)
print("spec mutated. process.args:", spec["process"]["args"][:2], "...")
print("namespaces:", [n["type"] for n in spec["linux"]["namespaces"]])
print("mounts (destinations):", [m["destination"] for m in spec["mounts"]])
PYEOF

echo "Final config.json (head):"
head -50 "$BUNDLE/config.json"

# ----------------------------------------------------------------------
hr "Phase 3: runsc run with production-mode bundle"
timeout 60 runsc --ignore-cgroups --network=sandbox run --bundle "$BUNDLE" tf-test
echo "runsc run exit=$?"

# ----------------------------------------------------------------------
hr "Phase 4: post-run — confirm host unchanged"
echo "Host /tmp/parent-secret.txt still intact:"
cat /tmp/parent-secret.txt
echo
echo "Host /data/worktree (should contain what sandbox left):"
ls -la /data/worktree

hr "All probes complete."
# Clean up background fake services so we don't leak them between runs
pkill -f "import http.server" 2>/dev/null || true
tail -f /dev/null
