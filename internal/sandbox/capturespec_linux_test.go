//go:build linux

package sandbox

import (
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// TestBuildCaptureSpec pins what makes the capture jail a jail: the tree is
// read-only, nothing is attached to its network namespace, it holds no
// capability, it runs as the tree owner's identity, and its program is the
// pinned subcommand over exactly the checkouts named.
func TestBuildCaptureSpec(t *testing.T) {
	uid := JailUID(7)
	spec, err := buildCaptureSpec(CaptureParams{Root: "/tmp/tf-run", Checkouts: []string{"acme/app/default", "acme/app/pr-7"}}, uid, uid, "/usr/local/bin/tf")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{TrustedTFBinaryDestination, "snapshot-capture", "/work", "acme/app/default", "acme/app/pr-7"}
	if strings.Join(spec.Process.Args, "|") != strings.Join(wantArgs, "|") {
		t.Errorf("args = %v, want %v", spec.Process.Args, wantArgs)
	}
	if spec.Process.User.UID != uint32(uid) || spec.Process.User.GID != uint32(uid) {
		t.Errorf("identity = %d:%d, want %d", spec.Process.User.UID, spec.Process.User.GID, uid)
	}
	caps := spec.Process.Capabilities
	if len(caps.Bounding)+len(caps.Effective)+len(caps.Permitted)+len(caps.Inheritable)+len(caps.Ambient) != 0 || !spec.Process.NoNewPrivileges {
		t.Errorf("capabilities = %+v, no_new_privs = %v", caps, spec.Process.NoNewPrivileges)
	}
	if !spec.Root.Readonly {
		t.Error("the root filesystem is writable")
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace && ns.Path != "" {
			t.Errorf("the capture joins network namespace %q; it must have one of its own", ns.Path)
		}
	}
	mounts := map[string]specs.Mount{}
	for _, m := range spec.Mounts {
		mounts[m.Destination] = m
	}
	for dest, src := range map[string]string{"/work": "/tmp/tf-run", TrustedTFBinaryDestination: "/usr/local/bin/tf"} {
		m, ok := mounts[dest]
		if !ok || m.Source != src {
			t.Errorf("mount %s = %+v, want source %s", dest, m, src)
			continue
		}
		ro := false
		for _, o := range m.Options {
			if o == "rw" {
				t.Errorf("mount %s is writable", dest)
			}
			ro = ro || o == "ro"
		}
		if !ro {
			t.Errorf("mount %s is not read-only: %v", dest, m.Options)
		}
	}
	for dest, m := range mounts {
		if m.Type == "bind" && dest != "/work" && dest != TrustedTFBinaryDestination {
			t.Errorf("unexpected bind mount %s <- %s: the jail may see only the tree and the TF binary", dest, m.Source)
		}
	}
	env := strings.Join(spec.Process.Env, "\n")
	for _, want := range []string{"GIT_NO_LAZY_FETCH=1", "HOME=/nonexistent"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

func TestBuildCaptureSpec_RefusesNonJailIdentity(t *testing.T) {
	for _, uid := range []int{0, 1000, ParkedRunTreeUID, SidecarUID(3)} {
		if _, err := buildCaptureSpec(CaptureParams{Root: "/tmp/x"}, uid, uid, "/bin/tf"); err == nil {
			t.Errorf("built a capture spec for uid %d", uid)
		}
	}
}

func TestNewCaptureContainerID_MatchesTheBootSweep(t *testing.T) {
	id, err := newCaptureContainerID()
	if err != nil {
		t.Fatal(err)
	}
	if !tfContainerIDRE.MatchString(id) {
		t.Errorf("capture container id %q would not be reaped by the boot sweep", id)
	}
}
