package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// SweepKeychain best-effort deletes the given keys directly from the OS
// keychain, bypassing the active-backend selection. cmd/uninstall uses it to
// remove keychain entries regardless of which backend the running config
// reads/writes: a box may still hold keychain rows from an earlier
// keychain-backed run even while TF_SECRETS_BACKEND=file is set now. A no-op
// when the keychain is unreachable — there's then nothing in it to remove, and
// the file backend's bag lives under the state root, swept by uninstall's
// data-dir wipe instead (so uninstall never needs TF_SECRET_ENCRYPTION_KEY).
// Missing entries are not an error. Each key goes through the chunk-aware
// delete, so every generation of an oversized value's chunks, and chunks
// orphaned by an interrupted write, are removed with it. A failure on one key
// does not stop the sweep of the others; the failures are returned joined.
func SweepKeychain(keys []string) error {
	if !probeKeychain() {
		return nil
	}
	var errs []error
	for _, k := range keys {
		if err := (keychainBackend{}).delete(k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// --- keychain chunking ---
//
// Some keychains cap the size of one entry (Windows Credential Manager via
// go-keyring: 2560 bytes), and a GitHub App private key or an OAuth envelope
// can exceed that. A value longer than chunkLimit is split by bytes into
// entries "<key>#<gen>#1".."<key>#<gen>#N", and the base entry holds the record
// "tfchunks:v2:<gen>:<N>:<sha256 of the whole value>" instead of the value. A
// value at or under the limit is stored exactly as given. chunkLimit 0 disables
// chunking, which keeps platforms without a practical cap on the plain path.
//
// <gen> names one write's chunks, so a new value's chunks never overwrite the
// ones the current base points at: a write puts every new chunk in place, then
// the base last (the commit), then removes the previous generation. A write
// that fails or is interrupted before the base leaves the previous value
// readable and removes only its own chunks. Records written by earlier builds,
// "tfchunks:v1:<N>:<digest>" with chunks "<key>#1".."<key>#N", still read and
// are replaced by the next write.
//
// Generations whose chunks may outlive their base are named in "<key>#orphans",
// a comma-separated list, so a later write, delete or uninstall sweep finds
// them: a generation's name cannot be guessed, and keychains cannot be listed.
// A write names the new generation before writing it and the current one
// before the base stops pointing at it; a delete names the current one before
// removing the base. A generation leaves the record only after its chunks are
// confirmed deleted, so a crash or a failed delete at any step leaves every
// chunk of both generations findable.

const (
	chunkBasePrefix   = "tfchunks:v2:"
	chunkBasePrefixV1 = "tfchunks:v1:"

	// maxChunks bounds a record's chunk count when it is parsed and the probes
	// for chunks no record names. A larger count is a corrupt record, and a
	// value that would need more is refused at write time.
	maxChunks = 64

	chunkGenLen = 8
)

// chunkLimit is the largest value stored in one keychain entry; 0 disables
// chunking. A var so tests can exercise the chunk logic on every OS.
var chunkLimit = platformChunkLimit

// keyringOps is the go-keyring surface the backend uses, held in a var so a
// test can inject a write failure.
type keyringOps struct {
	get    func(service, user string) (string, error)
	set    func(service, user, password string) error
	delete func(service, user string) error
}

var kr = keyringOps{get: keyring.Get, set: keyring.Set, delete: keyring.Delete}

// keychainMu serializes get/put/delete so a multi-entry write is never
// observed half-done by another goroutine in this process. Another process
// (an agent's `triagefactory exec`) can still read between a write's steps, which
// get tolerates by reading the base again.
//
// Writers are assumed to be single: one process writes a given key at a time,
// as the server does. There is no cross-process lock, because a keychain offers
// none. Two processes writing the same key at once can interleave their steps
// so that the base names a generation the other has already removed; reads of
// that key then fail loudly (a missing chunk or a digest mismatch) until the
// next write replaces it. The digest check means no value is read back wrong.
var keychainMu sync.Mutex

// errTornChunks marks a read that found the chunks and the base disagreeing:
// a chunk missing or the reassembled digest wrong. It is what a read racing a
// write in another process looks like, so get retries on it once.
var errTornChunks = errors.New("chunks do not match their record")

// chunkRecord is a parsed base record.
type chunkRecord struct {
	gen    string // "" for a v1 record, whose chunks are "<key>#<i>"
	n      int
	digest string
}

func isChunkRecord(base string) bool {
	return strings.HasPrefix(base, chunkBasePrefix) || strings.HasPrefix(base, chunkBasePrefixV1)
}

func chunkBaseRecord(gen string, n int, value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s%s:%d:%s", chunkBasePrefix, gen, n, hex.EncodeToString(sum[:]))
}

// parseChunkBase decodes a v1 or v2 base record.
func parseChunkBase(base string) (chunkRecord, error) {
	var rec chunkRecord
	var countField string
	if rest, ok := strings.CutPrefix(base, chunkBasePrefix); ok {
		parts := strings.Split(rest, ":")
		if len(parts) != 3 {
			return rec, errors.New("malformed chunk record")
		}
		if !validChunkGen(parts[0]) {
			return rec, fmt.Errorf("malformed chunk generation %q", parts[0])
		}
		rec.gen, countField, rec.digest = parts[0], parts[1], parts[2]
	} else if rest, ok := strings.CutPrefix(base, chunkBasePrefixV1); ok {
		parts := strings.Split(rest, ":")
		if len(parts) != 2 {
			return rec, errors.New("malformed chunk record")
		}
		countField, rec.digest = parts[0], parts[1]
	} else {
		return rec, errors.New("not a chunk record")
	}
	n, err := strconv.Atoi(countField)
	if err != nil || n < 1 {
		return rec, fmt.Errorf("malformed chunk count %q", countField)
	}
	if n > maxChunks {
		return rec, fmt.Errorf("chunk count %d is past the limit of %d", n, maxChunks)
	}
	rec.n = n
	return rec, nil
}

func validChunkGen(s string) bool {
	if len(s) != chunkGenLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// newChunkGen returns a random generation id not in avoid. It is random rather
// than derived from the value: an id derived from the content would give a
// re-write of the same value the names of the chunks the base already points
// at, so a failed re-write would delete the live value, and it would put part
// of the secret's hash in key names.
func newChunkGen(avoid []string) (string, error) {
	for {
		var b [chunkGenLen / 2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("keychain: generate chunk generation: %w", err)
		}
		if g := hex.EncodeToString(b[:]); !slices.Contains(avoid, g) {
			return g, nil
		}
	}
}

// chunkName is the keychain entry holding chunk i of a generation; gen "" is
// the v1 layout.
func chunkName(key, gen string, i int) string {
	if gen == "" {
		return fmt.Sprintf("%s#%d", key, i)
	}
	return fmt.Sprintf("%s#%s#%d", key, gen, i)
}

func orphanRecordName(key string) string { return key + "#orphans" }

// readOrphanGens returns the generations key#orphans names. Entries that are
// not generation ids are skipped with a warning.
func readOrphanGens(key string) ([]string, error) {
	raw, err := kr.get(service, orphanRecordName(key))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keychain read %s: %w", orphanRecordName(key), err)
	}
	var gens []string
	for g := range strings.SplitSeq(raw, ",") {
		switch {
		case g == "":
		case !validChunkGen(g):
			authLog.Warn("keychain orphan record holds a malformed generation; skipped", "key", key, "generation", g)
		case !slices.Contains(gens, g):
			gens = append(gens, g)
		}
	}
	return gens, nil
}

// writeOrphanRecord stores gens as key#orphans, or removes the record when gens
// is empty.
func writeOrphanRecord(key string, gens []string) error {
	name := orphanRecordName(key)
	if len(gens) == 0 {
		if err := kr.delete(service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return fmt.Errorf("keychain delete %s: %w", name, err)
		}
		return nil
	}
	if len(gens) > maxChunks {
		return fmt.Errorf("keychain: %d undeleted chunk generations are recorded for %s; remove them with `triagefactory uninstall` or delete the keychain entries by hand", len(gens), key)
	}
	if err := kr.set(service, name, strings.Join(gens, ",")); err != nil {
		return fmt.Errorf("keychain write %s: %w", name, err)
	}
	return nil
}

// readChunkState reads what a write or delete of key must clean up after: the
// current base's record, when it is a chunk record that parses, and the
// recorded orphan generations.
func readChunkState(key string) (*chunkRecord, []string, error) {
	var rec *chunkRecord
	base, err := kr.get(service, key)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
	case err != nil:
		return nil, nil, fmt.Errorf("keychain read %s: %w", key, err)
	case isChunkRecord(base):
		parsed, perr := parseChunkBase(base)
		if perr != nil {
			authLog.Warn("keychain chunk record is corrupt; its chunks are found by probing", "key", key, "error", perr)
		} else {
			rec = &parsed
		}
	}
	orph, err := readOrphanGens(key)
	if err != nil {
		return nil, nil, err
	}
	return rec, orph, nil
}

// deleteChunks removes chunks n..1 of a generation (descending, so a reader
// never sees a hole below a live chunk). Every entry is attempted, whatever
// happens to the others, and a missing entry is fine; failures are returned
// joined. With n = maxChunks it is the probe for chunks no record names, and
// it does not stop at the first gap.
func deleteChunks(key, gen string, n int) error {
	var errs []error
	for i := n; i >= 1; i-- {
		name := chunkName(key, gen, i)
		if err := kr.delete(service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, fmt.Errorf("keychain delete %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// liveGen is the generation a base record points at; "" for no record or a v1
// record, whose chunk names need no recording because the probes find them.
func liveGen(rec *chunkRecord) string {
	if rec == nil {
		return ""
	}
	return rec.gen
}

// recordGens adds gens ("" is skipped) to the recorded orphan generations orph
// and stores the result, writing nothing when there is nothing to add.
func recordGens(key string, orph []string, gens ...string) error {
	merged := slices.Clone(orph)
	for _, g := range gens {
		if g != "" && !slices.Contains(merged, g) {
			merged = append(merged, g)
		}
	}
	if len(merged) == len(orph) {
		return nil
	}
	return writeOrphanRecord(key, merged)
}

// unrecordGens puts the orphan record back to orph after a step that followed
// recordGens failed without changing the base, returning cause with any failure
// to do so joined to it. Leaving a still-live generation named is harmless: the
// next write or delete treats it as the live one first.
func unrecordGens(key string, orph []string, cause error) error {
	if err := writeOrphanRecord(key, orph); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// purgeChunks removes every chunk of key that a write or delete leaves
// without a base: those the old record names, every recorded orphan
// generation, and the v1 layout's names up to the cap. A generation leaves the
// orphan record only once all its chunks are confirmed deleted; one that could
// not be removed stays named so the next purge retries.
func purgeChunks(key string, rec *chunkRecord, orph []string) error {
	var errs []error
	var failed []string
	if rec != nil {
		if err := deleteChunks(key, rec.gen, rec.n); err != nil {
			errs = append(errs, err)
			if rec.gen != "" {
				failed = append(failed, rec.gen)
			}
		}
	}
	for _, g := range orph {
		if rec != nil && g == rec.gen {
			continue
		}
		if err := deleteChunks(key, g, maxChunks); err != nil {
			errs = append(errs, err)
			failed = append(failed, g)
		}
	}
	if err := deleteChunks(key, "", maxChunks); err != nil {
		errs = append(errs, err)
	}
	if err := writeOrphanRecord(key, failed); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// keychainBackend stores secrets in the OS keychain via go-keyring. The desktop
// default; selected whenever the keychain probe succeeds.
type keychainBackend struct{}

func (keychainBackend) get(key string) (string, error) {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	var torn error
	// A second pass re-reads the base: a write in another process may have
	// replaced the generation the first pass was reading.
	for range 2 {
		base, err := kr.get(service, key)
		if errors.Is(err, keyring.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if chunkLimit <= 0 || !isChunkRecord(base) {
			return base, nil
		}
		rec, err := parseChunkBase(base)
		if err != nil {
			return "", fmt.Errorf("keychain entry %s: %w", key, err)
		}
		value, err := readChunks(key, rec)
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, errTornChunks) {
			return "", err
		}
		torn = err
	}
	return "", torn
}

// readChunks reassembles a chunked value and checks it against its record's
// digest.
func readChunks(key string, rec chunkRecord) (string, error) {
	var sb strings.Builder
	for i := 1; i <= rec.n; i++ {
		part, err := kr.get(service, chunkName(key, rec.gen, i))
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("keychain entry %s: chunk %d of %d is missing: %w", key, i, rec.n, errTornChunks)
		}
		if err != nil {
			return "", fmt.Errorf("keychain read %s: %w", chunkName(key, rec.gen, i), err)
		}
		sb.WriteString(part)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	if hex.EncodeToString(sum[:]) != rec.digest {
		return "", fmt.Errorf("keychain entry %s: reassembled value does not match its recorded digest: %w", key, errTornChunks)
	}
	return sb.String(), nil
}

func (keychainBackend) put(key, value string) error {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	if chunkLimit <= 0 {
		return kr.set(service, key, value)
	}
	oldRec, orph, err := readChunkState(key)
	if err != nil {
		return err
	}

	if len(value) <= chunkLimit {
		// The base is the only pointer to the current generation's chunks, so
		// the generation is named before the base is overwritten.
		if err := recordGens(key, orph, liveGen(oldRec)); err != nil {
			return err
		}
		if err := kr.set(service, key, value); err != nil {
			return unrecordGens(key, orph, err)
		}
		// The value is stored; chunks of a longer value it replaced are
		// housekeeping, so their removal failing is logged, not returned.
		if err := purgeChunks(key, oldRec, orph); err != nil {
			authLog.Warn("keychain value stored; chunks of the previous value could not be removed", "key", key, "error", err)
		}
		return nil
	}

	n := (len(value) + chunkLimit - 1) / chunkLimit
	if n > maxChunks {
		return fmt.Errorf("keychain value for %s needs %d chunks, more than the %d allowed", key, n, maxChunks)
	}
	avoid := slices.Clone(orph)
	if oldRec != nil && oldRec.gen != "" {
		avoid = append(avoid, oldRec.gen)
	}
	gen, err := newChunkGen(avoid)
	if err != nil {
		return err
	}
	// Name the new generation before writing any of it, so chunks left by a
	// crash before the base write can still be found and removed, and the
	// current one too, since the base write drops the only other pointer to it.
	if err := recordGens(key, orph, gen, liveGen(oldRec)); err != nil {
		return err
	}
	for i := 1; i <= n; i++ {
		lo := (i - 1) * chunkLimit
		hi := min(lo+chunkLimit, len(value))
		if err := kr.set(service, chunkName(key, gen, i), value[lo:hi]); err != nil {
			return abandonChunkedPut(key, gen, i, orph, fmt.Errorf("keychain write %s: %w", chunkName(key, gen, i), err))
		}
	}
	if err := kr.set(service, key, chunkBaseRecord(gen, n, value)); err != nil {
		return abandonChunkedPut(key, gen, n, orph, fmt.Errorf("keychain write %s: %w", key, err))
	}

	// The new generation is live. Retire what the previous value left; a
	// failure here leaves the new value stored, so it is logged, and the
	// generations that could not be removed stay named for the next attempt.
	if err := purgeChunks(key, oldRec, orph); err != nil {
		authLog.Warn("keychain value stored; chunks of the previous value could not be removed", "key", key, "error", err)
	}
	return nil
}

// abandonChunkedPut cleans up after a chunked write that failed before its base
// was written: it removes the new generation's chunks (1..attempted) and takes
// the generation back out of the orphan record. The base and the previous
// generation's chunks are untouched, so the previous value stays readable. The
// original error is returned; a cleanup failure is joined to it, and the new
// generation then stays named in the orphan record for a later purge.
func abandonChunkedPut(key, gen string, attempted int, orph []string, cause error) error {
	errs := []error{cause}
	if err := deleteChunks(key, gen, attempted); err != nil {
		return errors.Join(append(errs, err)...)
	}
	if err := writeOrphanRecord(key, orph); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// delete removes the base first, so a failure there leaves the value intact
// and readable, and a failure after it leaves only orphan chunks, which the
// orphan record and the probes find again. Then it removes the chunks the base
// named, and probes for the rest.
func (keychainBackend) delete(key string) error {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	if chunkLimit <= 0 {
		if err := kr.delete(service, key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return fmt.Errorf("keychain delete %s: %w", key, err)
		}
		return nil
	}
	rec, orph, err := readChunkState(key)
	if err != nil {
		return err
	}
	// The base is the only pointer to the current generation's chunks, so the
	// generation is named before the base goes: a crash between the two would
	// otherwise leave chunks no later delete or sweep can name.
	if err := recordGens(key, orph, liveGen(rec)); err != nil {
		return err
	}
	if err := kr.delete(service, key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return unrecordGens(key, orph, fmt.Errorf("keychain delete %s: %w", key, err))
	}
	return purgeChunks(key, rec, orph)
}
