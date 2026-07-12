#!/bin/sh
# Follow-on to probe-browser.sh: settle the §8 acceptance Q1 ("does the OCI
# seccomp even enforce?") that the browser probe surfaced — chromium's
# clone-based threads ran fine under our no-clone `prod` profile, which is
# only possible if (a) gVisor doesn't enforce the OCI app-seccomp list, or
# (b) clone slips through. This answers it directly and cheaply (python3-only
# rootfs, no chromium) by calling DENIED, CAPLESS syscalls under seccomp=none
# vs seccomp=prod (our real list from syscalls.go) and diffing errno.
#
#   getpid (allowed control) -> must succeed in every run (harness sanity)
#
# ERRNO modes (prod/denyall/denygetpid) are NOT dispositive: gVisor systrap
# intercepts serviced syscalls with seccomp RET_TRAP, which OUTRANKS RET_ERRNO,
# so an ERRNO rule is shadowed for anything the Sentry services.
#
# KILL modes (killall/killwrite) ARE dispositive: SCMP_ACT_KILL_PROCESS is the
# highest-precedence action and cannot be shadowed. If runsc applied the OCI
# profile, a KILL rule would terminate the process on its first (or first
# write()) syscall; if the rows still print, the profile is inert.
#
# Zero caps in BOTH modes (matching spec.go), so cap-gated syscalls are avoided
# as discriminators.
#
# Reproduce (host needs docker + runsc on PATH):
#   docker run --rm --privileged \
#     -v "$(git rev-parse --show-toplevel)":/src \
#     -v /usr/local/bin/runsc:/usr/local/bin/runsc:ro \
#     -w /src alpine:3.20 \
#     sh /src/docs/for-agents/specs/playwright-chromium-sandbox/probe-seccomp.sh
set -eu
ALPINE_V=3.20
BUNDLE=/bundle; ROOT="$BUNDLE/rootfs"
banner(){ printf '\n\n######## %s ########\n' "$*"; }

banner "0. tooling + minimal rootfs (busybox + python3)"
apk add --no-cache python3 >/dev/null
echo "runsc: $(runsc --version | head -1)"
rm -rf "$BUNDLE"; mkdir -p "$ROOT/etc/apk/keys"
cp -a /etc/apk/keys/. "$ROOT/etc/apk/keys/"
printf 'https://dl-cdn.alpinelinux.org/alpine/v%s/main\n' "$ALPINE_V" > "$ROOT/etc/apk/repositories"
apk add --root "$ROOT" --initdb --no-cache alpine-baselayout busybox musl python3 >/dev/null
echo "rootfs ready ($(du -sh "$ROOT" | cut -f1))"

# in-sandbox syscall probe (x86_64 numbers). all-zero args are harmless
# for these (clone3 NULL->EFAULT, unshare(0)->no-op, userfaultfd 0->fd/EPERM).
PROBE='import ctypes,os
libc=ctypes.CDLL(None,use_errno=True)
tests=[("getpid",39,"allowed-control"),("unshare(0)",272,"denied-capless"),("clone3",435,"denied-capless"),("userfaultfd",323,"denied-capless"),("setns",308,"denied-capless")]
for nm,n,kind in tests:
    ctypes.set_errno(0)
    r=libc.syscall(n,0,0,0,0,0,0)
    e=ctypes.get_errno()
    print("SYS %-12s n=%-4d %-16s ret=%-6d errno=%d(%s)"%(nm,n,kind,r,e,os.strerror(e) if e else "OK"))
'

run(){
  label=$1; mode=$2
  rm -f "$BUNDLE/config.json"; ( cd "$BUNDLE" && runsc spec )
  SECCOMP_MODE="$mode" PROBE="$PROBE" python3 - "$BUNDLE/config.json" <<'PY'
import json,os,re,sys
cfgp=sys.argv[1]; mode=os.environ["SECCOMP_MODE"]
spec=json.load(open(cfgp)); p=spec["process"]
p["terminal"]=False
p["args"]=["/usr/bin/python3","-c",os.environ["PROBE"]]
p["user"]={"uid":10000,"gid":10000}; p["cwd"]="/"
p["noNewPrivileges"]=True
p["env"]=["PATH=/usr/bin:/bin","HOME=/tmp"]
p["capabilities"]={k:[] for k in("bounding","effective","permitted","inheritable","ambient")}
spec["root"]={"path":"rootfs","readonly":True}
ARCHS=["SCMP_ARCH_X86_64","SCMP_ARCH_X86","SCMP_ARCH_AARCH64"]
if mode=="none":
    spec["linux"].pop("seccomp",None)
elif mode=="denyall":
    # positive control: default-deny, ZERO allows. A real filter makes the
    # process unable to issue a single syscall -> it cannot run at all.
    spec["linux"]["seccomp"]={"defaultAction":"SCMP_ACT_ERRNO","architectures":ARCHS,"syscalls":[]}
elif mode=="denygetpid":
    # positive control: allow-by-default but explicitly ERRNO just getpid.
    # If enforced, the getpid control row flips to EPERM(1). Independent of
    # the syscalls.go extraction, so it can't be a list-parsing artifact.
    spec["linux"]["seccomp"]={"defaultAction":"SCMP_ACT_ALLOW","architectures":ARCHS,
      "syscalls":[{"names":["getpid"],"action":"SCMP_ACT_ERRNO"}]}
elif mode=="killall":
    # DISPOSITIVE: KILL_PROCESS outranks gVisor systrap's RET_TRAP, so it
    # cannot be shadowed the way an ERRNO default can. If runsc applies the
    # OCI profile at all, the app is killed on its first syscall (no output).
    spec["linux"]["seccomp"]={"defaultAction":"SCMP_ACT_KILL_PROCESS","architectures":ARCHS,"syscalls":[]}
elif mode=="killwrite":
    # DISPOSITIVE positive control: allow-by-default, KILL just write(). If
    # applied, python dies at its first print (no SYS rows). KILL > TRAP, so
    # unlike denygetpid this is NOT shadowed by gVisor's trap-to-Sentry.
    spec["linux"]["seccomp"]={"defaultAction":"SCMP_ACT_ALLOW","architectures":ARCHS,
      "syscalls":[{"names":["write"],"action":"SCMP_ACT_KILL_PROCESS"}]}
else:
    src=open("/src/internal/sandbox/syscalls.go").read()
    blk=src.split("defaultAllowedSyscalls = []string{",1)[1].split("\n}",1)[0]
    names=re.findall(r'"([^"]+)"',blk)
    spec["linux"]["seccomp"]={"defaultAction":"SCMP_ACT_ERRNO","architectures":ARCHS,
      "syscalls":[{"names":sorted(set(names)),"action":"SCMP_ACT_ALLOW"}]}
json.dump(spec,open(cfgp,"w"),indent=2)
print("config seccomp=%s"%mode)
PY
  banner "RUN seccomp=$mode"
  set +e
  timeout 60 runsc --platform=systrap --ignore-cgroups --network=sandbox run --bundle "$BUNDLE" "tfsec-$label"
  rc=$?
  set -e
  runsc --ignore-cgroups delete --force "tfsec-$label" 2>/dev/null || true
  echo "==== EXIT seccomp=$mode rc=$rc ===="
}

run none none
run prod prod
run denyall denyall
run denygetpid denygetpid
run killall killall
run killwrite killwrite

banner "INTERPRETATION"
echo "ERRNO modes (prod/denyall/denygetpid) can be SHADOWED: gVisor systrap traps serviced"
echo "syscalls with RET_TRAP, which OUTRANKS RET_ERRNO, so an ERRNO rule is invisible for any"
echo "syscall the Sentry services. Those rows alone are NOT dispositive about enforcement."
echo
echo "KILL modes ARE dispositive (SCMP_ACT_KILL_PROCESS outranks RET_TRAP, cannot be shadowed):"
echo " - killall/killwrite STILL print SYS rows      => runsc does NOT apply the OCI profile (inert)."
echo " - killall/killwrite print NO rows + killed/rc!=0 => runsc DOES apply it (real trap-escape backstop)."
