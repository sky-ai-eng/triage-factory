//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// The per-run filesystem grants a cell carries besides its run tree: the two
// socket directories under the socket root and the staging directories a
// launch mounts. Each names the run's own jail gid, so each is created or
// granted for one engagement and taken back before that engagement's index is
// released (RunNetwork.Close).

// runSocketDirMode is a run's socket directory: its sidecar (owner) creates
// and serves the agenthost socket and the gh-injector cert inside it, the
// run's jail gid may traverse it, and nobody else may do anything. The socket
// root above it is root-owned and not group-writable, so this directory is the
// only place a sidecar can create an entry at all.
const runSocketDirMode = 0o750

// toolHostSocketDirMode is a run's tool-host socket directory: the
// orchestrator (owner) binds the socket, the jail gid may traverse it, and the
// setgid bit gives the socket the jail gid at birth — the orchestrator carries
// no jail gid, so it could not chgrp the socket to one itself.
const toolHostSocketDirMode = 0o2750

// stagingDirMode is a launch's staging directory once it is granted: the
// orchestrator (owner) keeps writing it, the run's jail gid reads it through
// its read-only mount, and nobody else can reach what is inside.
const stagingDirMode = 0o750

// orchestratorUID is the uid of the process that creates run trees, staging
// and the tool-host socket: the registered orchestrator when this runs inside
// the broker, this process otherwise.
func orchestratorUID() int {
	if runTreeOwnerExtraUID >= 0 {
		return runTreeOwnerExtraUID
	}
	return os.Getuid()
}

// PrepareRunSockets creates conversationID's two per-run socket directories
// for the engagement at subnetIdx, replacing whatever an earlier engagement of
// the same conversation left at those paths.
//
// Replacing is the point, not hygiene. The paths are keyed by conversation, so
// sequential engagements share them, and the engagement that leaves its
// directories behind is the one that was killed. Removing and recreating them
// here — before the sidecar is launched, from the one process that may write
// in the socket root — is what lets a re-claim that races its predecessor's
// teardown come up, and what gives every file inside the new directories an
// owner and group of this engagement's.
func (hostOps) PrepareRunSockets(_ context.Context, conversationID string, subnetIdx uint8) error {
	if err := validateOpaqueID("run id", conversationID); err != nil {
		return err
	}
	if err := os.MkdirAll(trustedAgentHostSocketRoot, 0o711); err != nil {
		return fmt.Errorf("sandbox: create socket root: %w", err)
	}
	dirs := []struct {
		path      string
		uid, gid  int
		mode      uint32
		ownerRole string
	}{
		{RunSocketDir(conversationID), SidecarUID(subnetIdx), JailGID(subnetIdx), runSocketDirMode, "sidecar"},
		{TrustedToolHostSocketDir(conversationID), orchestratorUID(), JailGID(subnetIdx), toolHostSocketDirMode, "orchestrator"},
	}
	for _, d := range dirs {
		if IsReservedSocketRootPath(d.path) {
			return fmt.Errorf("sandbox: run id %q derives onto a reserved socket root entry", conversationID)
		}
		if err := os.RemoveAll(d.path); err != nil {
			return fmt.Errorf("sandbox: clear a predecessor's socket directory %s: %w", d.path, err)
		}
		if err := makeOwnedDir(d.path, d.uid, d.gid, d.mode); err != nil {
			return fmt.Errorf("sandbox: create the %s's socket directory: %w", d.ownerRole, err)
		}
	}
	return nil
}

// makeOwnedDir creates path and gives it owner, group and mode through a
// descriptor pinned on the directory it just made, so the ownership lands on
// that inode and nothing else.
func makeOwnedDir(path string, uid, gid int, mode uint32) error {
	if err := unix.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer closeFd(fd)
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// ReleaseRunSockets removes the socket directories PrepareRunSockets made for
// the engagement at subnetIdx. Each is removed only while it still carries
// this engagement's identity — the sidecar directory its sidecar uid, the
// tool-host directory its jail gid — because a successor engagement of the
// same conversation may already have replaced them, and removing its live
// directories would take its sockets away mid-run. Exact for the reason
// RunNetwork.Close states: no other engagement can hold this index until this
// call has returned. A missing directory is success.
func (hostOps) ReleaseRunSockets(_ context.Context, conversationID string, subnetIdx uint8) error {
	if err := validateOpaqueID("run id", conversationID); err != nil {
		return err
	}
	var errs []error
	for _, d := range []struct {
		path string
		held func(*syscall.Stat_t) bool
	}{
		{RunSocketDir(conversationID), func(st *syscall.Stat_t) bool { return int(st.Uid) == SidecarUID(subnetIdx) }},
		{TrustedToolHostSocketDir(conversationID), func(st *syscall.Stat_t) bool { return int(st.Gid) == JailGID(subnetIdx) }},
	} {
		if IsReservedSocketRootPath(d.path) {
			continue
		}
		info, err := os.Lstat(d.path)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || !d.held(st) {
			continue
		}
		if err := os.RemoveAll(d.path); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: remove socket directory %s: %w", d.path, err))
		}
	}
	return errors.Join(errs...)
}

// reapOrphanRunSockets removes every per-run entry under the socket root.
// Part of the boot sweep, and unconditional for the reason that sweep's other
// halves are: it runs once, before any run exists, so everything here belongs
// to an engagement that is gone — whose sidecar uid and jail gid this boot's
// allocator is about to hand out again. Only the broker's own socket stays.
func reapOrphanRunSockets() {
	entries, err := os.ReadDir(trustedAgentHostSocketRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(trustedAgentHostSocketRoot, e.Name())
		if IsReservedSocketRootPath(path) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			sandboxLog.Warn("orphan per-run socket entry not removed", "path", path, "error", err)
		}
	}
}

// GrantRunStaging gives a launch's staging directory to the jail at subnetIdx:
// group JailGID(subnetIdx), mode stagingDirMode. Shallow, because the
// directory's own mode is what decides who reaches anything inside it.
//
// The orchestrator creates staging and owns it, but carries no jail gid, so it
// cannot make this grant itself. The checks are what keep the grant from
// becoming a way to hand an arbitrary directory to a jail: the directory must
// be exactly one level under a staging base, a real directory (the final
// component is opened O_NOFOLLOW off a descriptor on the base), and owned by
// the orchestrator.
//
// Taking the grant back is the orchestrator's own act — an owner may always
// chgrp to its own group — which RunNetwork.Close does before the index goes.
func (hostOps) GrantRunStaging(_ context.Context, dir string, subnetIdx uint8) error {
	if err := validateAbsCleanPath("staging dir", dir); err != nil {
		return err
	}
	base, name := filepath.Dir(dir), filepath.Base(dir)
	if base != SkillStagingBase() && base != MemoryStagingBase() {
		return fmt.Errorf("sandbox: grant staging: %s is not under a staging base", dir)
	}
	if err := validateOpaqueID("staging dir name", name); err != nil {
		return err
	}
	realBase, err := realPath(base)
	if err != nil {
		return fmt.Errorf("sandbox: grant staging: %w", err)
	}
	baseFd, err := unix.Open(realBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("sandbox: grant staging: open %s: %w", realBase, err)
	}
	defer closeFd(baseFd)
	fd, err := openat2AnchoredChildDir(baseFd, name)
	if err != nil {
		return fmt.Errorf("sandbox: grant staging: open %s: %w", dir, err)
	}
	defer closeFd(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("sandbox: grant staging: fstat %s: %w", dir, err)
	}
	if int(st.Uid) != orchestratorUID() {
		return fmt.Errorf("sandbox: grant staging: %s is owned by uid %d, not the orchestrator", dir, st.Uid)
	}
	if err := unix.Fchown(fd, -1, JailGID(subnetIdx)); err != nil {
		return fmt.Errorf("sandbox: grant staging: chgrp %s: %w", dir, err)
	}
	if err := unix.Fchmod(fd, stagingDirMode); err != nil {
		return fmt.Errorf("sandbox: grant staging: chmod %s: %w", dir, err)
	}
	return nil
}

// revokeRunStaging takes a staging grant back: the directory's group returns
// to this process's own, which the owner may set without any privilege. A
// directory that is already gone has nothing left to revoke.
func revokeRunStaging(dir string) error {
	err := os.Chown(dir, -1, os.Getgid())
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
