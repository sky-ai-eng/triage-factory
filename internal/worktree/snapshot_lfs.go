package worktree

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// LFSObject is one Git LFS object a snapshot carries: the content of a file
// the repository keeps in Git LFS, named by its object id, the sha256 of that
// content in hex.
type LFSObject struct {
	OID string
	// Path is where the object's bytes are read from: the checkout's own
	// object file when the capture ran in this process, and the file the
	// orchestrator staged from the capture jail's stream, or from a snapshot
	// blob, otherwise.
	Path string
}

// lfsPointerMaxSize is the size below which git-lfs reads a blob as a
// possible pointer. A pointer is about 130 bytes.
const lfsPointerMaxSize = 1024

// lfsPointerVersion is the first line of every pointer git-lfs writes.
const lfsPointerVersion = "version https://git-lfs.github.com/spec/v1"

var lfsOIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidLFSOID reports whether oid is a Git LFS object id: 64 lowercase hex
// digits.
func ValidLFSOID(oid string) bool { return lfsOIDPattern.MatchString(oid) }

// lfsPointer is a parsed Git LFS pointer.
type lfsPointer struct {
	oid  string
	size int64
}

// parseLFSPointer reads data as a Git LFS pointer, as git-lfs does: the
// version line first, then key-value lines, among them the sha256 oid and the
// size. Extension lines and keys git-lfs may add later are ignored.
func parseLFSPointer(data []byte) (lfsPointer, bool) {
	if len(data) >= lfsPointerMaxSize || !bytes.HasSuffix(data, []byte("\n")) {
		return lfsPointer{}, false
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lines[0] != lfsPointerVersion {
		return lfsPointer{}, false
	}
	var p lfsPointer
	sized := false
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return lfsPointer{}, false
		}
		switch key {
		case "oid":
			oid, ok := strings.CutPrefix(value, "sha256:")
			if !ok || !ValidLFSOID(oid) {
				return lfsPointer{}, false
			}
			p.oid = oid
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return lfsPointer{}, false
			}
			p.size, sized = n, true
		}
	}
	return p, p.oid != "" && sized
}

// lfsObjectPath is where git-lfs keeps an object under objectsDir.
func lfsObjectPath(objectsDir, oid string) string {
	return filepath.Join(objectsDir, oid[0:2], oid[2:4], oid)
}

// lfsObjectsDir is the checkout's Git LFS object store: lfs/objects in the
// repository's common git dir, so a linked worktree's objects are in the bare
// it shares, as git-lfs keeps them. lfs.storage, which moves the store, is not
// read: in a checkout the config is the agent's to write, and what the capture
// reads stays inside the repository.
func lfsObjectsDir(ctx context.Context, dir string) (string, error) {
	out, err := gitCapture(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("locate git common dir: %w", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "lfs", "objects"), nil
}

// lfsStoreExists reports whether the LFS store at objectsDir is there. git-lfs
// creates it with the first object it stores, so a checkout without one holds
// no LFS object, and the capture's LFS steps have nothing to do in it.
func lfsStoreExists(objectsDir string) bool {
	_, err := os.Lstat(objectsDir)
	return !errors.Is(err, fs.ErrNotExist)
}

// unpushedLFSObjects lists the Git LFS objects named by pointers in the
// commits a snapshot's bundle carries (rev --not --remotes), which the
// checkout holds and nothing upstream may have. It scans the range as `git lfs
// pre-push` does: every blob in it under the pointer size cutoff, read as a
// pointer. An object the checkout does not hold, or holds at a size its
// pointer does not name, is left out: what the store has there is not the
// object.
//
// --missing=allow-any keeps the walk from fetching a blob a partial clone
// lacks. Such a blob came from upstream, so whatever object it names is not
// the run's to carry.
func unpushedLFSObjects(ctx context.Context, wtPath, rev string) ([]LFSObject, error) {
	objectsDir, err := lfsObjectsDir(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	// The walk costs what the bundle's does, so a repository that keeps
	// nothing in Git LFS does not pay for it twice.
	if !lfsStoreExists(objectsDir) {
		return nil, nil
	}
	out, err := gitCapture(ctx, wtPath, nil, "rev-list", "--objects", "--no-object-names", "--missing=allow-any", rev, "--not", "--remotes", "--")
	if err != nil {
		return nil, fmt.Errorf("git rev-list --objects: %w", err)
	}
	blobs, err := smallBlobs(ctx, wtPath, nil, strings.Fields(string(out)))
	if err != nil {
		return nil, err
	}
	pointers, err := readLFSPointers(ctx, wtPath, nil, blobs)
	if err != nil {
		return nil, err
	}
	if len(pointers) == 0 {
		return nil, nil
	}
	sizes := map[string]int64{}
	for _, p := range pointers {
		sizes[p.oid] = p.size
	}
	oids := make([]string, 0, len(sizes))
	for oid := range sizes {
		oids = append(oids, oid)
	}
	slices.Sort(oids)
	var objects []LFSObject
	for _, oid := range oids {
		path := lfsObjectPath(objectsDir, oid)
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat LFS object %s: %w", oid, err)
		}
		if !fi.Mode().IsRegular() || fi.Size() != sizes[oid] {
			continue
		}
		objects = append(objects, LFSObject{OID: oid, Path: path})
	}
	return objects, nil
}

// smallBlobs asks git about each object name in names and returns, in the
// same order, the blob id of each that is a blob small enough to be a Git LFS
// pointer, and "" for each that is not (or that git does not hold). Asking
// does not fetch: the names are of objects git already holds, or, in a
// partial clone, of blobs a checkout read.
func smallBlobs(ctx context.Context, dir string, env []string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var check bytes.Buffer
	if err := gitCaptureIO(ctx, dir, env, strings.NewReader(strings.Join(names, "\n")+"\n"), &check,
		"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)"); err != nil {
		return nil, fmt.Errorf("git cat-file --batch-check: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(check.String(), "\n"), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("git cat-file --batch-check answered %d of %d names", len(lines), len(names))
	}
	blobs := make([]string, len(names))
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		if n, err := strconv.ParseInt(f[2], 10, 64); err == nil && n < lfsPointerMaxSize {
			blobs[i] = f[0]
		}
	}
	return blobs, nil
}

// readLFSPointers reads each of blobs, small blob ids smallBlobs returned, and
// returns the ones that are Git LFS pointers, keyed by blob id. "" entries are
// skipped.
func readLFSPointers(ctx context.Context, dir string, env []string, blobs []string) (map[string]lfsPointer, error) {
	var ids []string
	for _, b := range blobs {
		if b != "" {
			ids = append(ids, b)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var contents bytes.Buffer
	if err := gitCaptureIO(ctx, dir, env, strings.NewReader(strings.Join(ids, "\n")+"\n"), &contents, "cat-file", "--batch"); err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	pointers := map[string]lfsPointer{}
	br := bufio.NewReader(&contents)
	for {
		header, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) && header == "" {
			return pointers, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read cat-file --batch: %w", err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("read cat-file --batch: unexpected header %q", strings.TrimSpace(header))
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 0 || n >= lfsPointerMaxSize {
			return nil, fmt.Errorf("read cat-file --batch: unexpected header %q", strings.TrimSpace(header))
		}
		body := make([]byte, n+1)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, fmt.Errorf("read cat-file --batch: %w", err)
		}
		if p, ok := parseLFSPointer(body[:n]); ok {
			pointers[f[0]] = p
		}
	}
}

// installLFSObjects writes each object into the Git LFS store of the checkout
// at wtDir, where git-lfs finds it, so the run's next push uploads it. An
// object is installed only as content that hashes to its id, and one that does
// not is left out with a warning, since the snapshot does not hold that
// object. An object the store already holds is kept as it is.
func installLFSObjects(ctx context.Context, wtDir string, objects []LFSObject) error {
	if len(objects) == 0 {
		return nil
	}
	objectsDir, err := lfsObjectsDir(ctx, wtDir)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if !ValidLFSOID(obj.OID) {
			return fmt.Errorf("install LFS object: %q is not an object id", obj.OID)
		}
		if err := installLFSObject(objectsDir, obj); err != nil {
			return fmt.Errorf("install LFS object %s: %w", obj.OID, err)
		}
	}
	return nil
}

func installLFSObject(objectsDir string, obj LFSObject) (err error) {
	dest := lfsObjectPath(objectsDir, obj.OID)
	if _, err := os.Lstat(dest); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	src, err := os.Open(obj.Path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".restore-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != obj.OID {
		_ = os.Remove(tmp.Name())
		worktreeLog.Warn("snapshot's LFS object does not hash to its id; it is not restored", "oid", obj.OID, "sha256", got)
		return nil
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// dropFetchedLFSPaths returns paths without the LFS files the run fetched:
// every file whose HEAD blob is a Git LFS pointer and whose content is the
// object that pointer names. git-lfs would clean such a file back into the
// same pointer, so nothing about it changed. But the capture's git runs
// without git-lfs's filter, and reads the file as modified whenever the index
// cannot vouch for it by stat alone, which it cannot for a file written in the
// same second as the index: every file `git lfs pull` writes, until the index
// is next rewritten. Staged, the file would be hashed and stored whole, and
// ride in the patch whole. Left out, the restored checkout holds its pointer,
// and the run fetches the object again.
//
// Best-effort: on a failure paths come back as they were, and a fetched file
// then travels whole, which restores the same tree. A path holding a newline
// cannot be named on cat-file's input and stays in.
func dropFetchedLFSPaths(ctx context.Context, wtPath string, paths []string) []string {
	if objectsDir, err := lfsObjectsDir(ctx, wtPath); err != nil || !lfsStoreExists(objectsDir) {
		return paths
	}
	var named []string
	var names []string
	for _, p := range paths {
		if !strings.Contains(p, "\n") {
			named = append(named, p)
			names = append(names, "HEAD:"+p)
		}
	}
	blobs, err := smallBlobs(ctx, wtPath, nil, names)
	if err != nil {
		return paths
	}
	pointers, err := readLFSPointers(ctx, wtPath, nil, blobs)
	if err != nil || len(pointers) == 0 {
		return paths
	}
	fetched := map[string]bool{}
	for i, p := range named {
		if ptr, ok := pointers[blobs[i]]; ok && fileHoldsLFSObject(filepath.Join(wtPath, filepath.FromSlash(p)), ptr) {
			fetched[p] = true
		}
	}
	if len(fetched) == 0 {
		return paths
	}
	kept := make([]string, 0, len(paths)-len(fetched))
	for _, p := range paths {
		if !fetched[p] {
			kept = append(kept, p)
		}
	}
	return kept
}

// unstageFetchedLFSFiles takes the LFS files the run fetched (as
// dropFetchedLFSPaths finds them) back out of a capture's temp index once the
// full `add -A` has staged them, which leaves them out of the patch.
//
// Best-effort: a failure leaves the index as it was, and a file then travels
// whole, which restores the same tree.
func unstageFetchedLFSFiles(ctx context.Context, wtPath string, env []string) {
	if objectsDir, err := lfsObjectsDir(ctx, wtPath); err != nil || !lfsStoreExists(objectsDir) {
		return
	}
	out, err := gitCapture(ctx, wtPath, env, "diff", "--no-ext-diff", "--no-textconv", "--cached",
		"--raw", "-z", "--no-abbrev", "--no-renames", "--diff-filter=M", "HEAD")
	if err != nil {
		return
	}
	type entry struct{ mode, blob, path string }
	var entries []entry
	var blobs []string
	// Each record is ":<mode> <mode> <blob> <blob> M" and then its path, each
	// NUL-terminated.
	tokens := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(tokens); i += 2 {
		f := strings.Fields(strings.TrimPrefix(tokens[i], ":"))
		if len(f) != 5 || f[0] != f[1] || (f[0] != "100644" && f[0] != "100755") {
			continue
		}
		entries = append(entries, entry{mode: f[0], blob: f[2], path: tokens[i+1]})
		blobs = append(blobs, f[2])
	}
	small, err := smallBlobs(ctx, wtPath, env, blobs)
	if err != nil {
		return
	}
	pointers, err := readLFSPointers(ctx, wtPath, env, small)
	if err != nil || len(pointers) == 0 {
		return
	}
	var headEntries bytes.Buffer
	for _, e := range entries {
		p, ok := pointers[e.blob]
		if ok && fileHoldsLFSObject(filepath.Join(wtPath, filepath.FromSlash(e.path)), p) {
			fmt.Fprintf(&headEntries, "%s %s\t%s\x00", e.mode, e.blob, e.path)
		}
	}
	if headEntries.Len() == 0 {
		return
	}
	_ = gitCaptureIO(ctx, wtPath, env, &headEntries, io.Discard, "update-index", "-z", "--index-info")
}

// fileHoldsLFSObject reports whether the regular file at path is the object p
// names.
func fileHoldsLFSObject(path string, p lfsPointer) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != p.size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == p.oid
}
