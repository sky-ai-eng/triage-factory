package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// runConformance is the single behavioral contract every Storage backend
// must satisfy: round-trip, overwrite, idempotent delete, exists, the
// missing-key signal, and a large streamed blob. fs_test.go runs it
// against fsStorage on a temp dir; object_test.go runs it against
// objectStorage on a throwaway SeaweedFS container. Anything backend-specific
// (filesystem traversal rejection, env parsing) lives in the per-backend
// test files, not here.
func runConformance(t *testing.T, store Storage) {
	t.Helper()
	ctx := context.Background()

	t.Run("round trip + exists", func(t *testing.T) {
		key := "org-1/run-1/note.txt"
		want := []byte("the workspace handoff note")
		if err := store.Put(ctx, key, bytes.NewReader(want)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		ok, err := store.Exists(ctx, key)
		if err != nil || !ok {
			t.Fatalf("Exists after Put = %v, %v; want true, nil", ok, err)
		}
		if got := mustGet(t, store, key); !bytes.Equal(got, want) {
			t.Fatalf("Get round-trip = %q; want %q", got, want)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		key := "org-1/run-2/obj"
		if err := store.Put(ctx, key, bytes.NewReader([]byte("first"))); err != nil {
			t.Fatalf("Put first: %v", err)
		}
		if err := store.Put(ctx, key, bytes.NewReader([]byte("second-and-longer"))); err != nil {
			t.Fatalf("Put second: %v", err)
		}
		if got := mustGet(t, store, key); string(got) != "second-and-longer" {
			t.Fatalf("Get after overwrite = %q; want %q", got, "second-and-longer")
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		key := "org-1/run-3/obj"
		if err := store.Put(ctx, key, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		ok, err := store.Exists(ctx, key)
		if err != nil || ok {
			t.Fatalf("Exists after Delete = %v, %v; want false, nil", ok, err)
		}
		// Deleting an already-absent key must succeed (S3 DELETE semantics).
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("Delete (second) = %v; want nil", err)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		key := "org-1/does-not-exist"
		ok, err := store.Exists(ctx, key)
		if err != nil || ok {
			t.Fatalf("Exists(missing) = %v, %v; want false, nil", ok, err)
		}
		rc, err := store.Get(ctx, key)
		if rc != nil {
			rc.Close()
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(missing) err = %v; want ErrNotFound", err)
		}
	})

	t.Run("get range", func(t *testing.T) {
		key := "org-1/run-5/ranged.bin"
		body := []byte("0123456789abcdef")
		if err := store.Put(ctx, key, bytes.NewReader(body)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Bounded window in the middle: bytes [5, 9).
		rc, err := store.GetRange(ctx, key, 5, 4)
		if err != nil {
			t.Fatalf("GetRange bounded: %v", err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read bounded range: %v", err)
		}
		if want := body[5:9]; !bytes.Equal(got, want) {
			t.Fatalf("GetRange(5,4) = %q; want %q", got, want)
		}
		// Zero-length window: an empty read, never a backend InvalidRange error.
		rc, err = store.GetRange(ctx, key, 4, 0)
		if err != nil {
			t.Fatalf("GetRange zero-length: %v", err)
		}
		got, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zero-length range: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("GetRange(4,0) = %q; want empty", got)
		}
		// To-end from an offset: length < 0.
		rc, err = store.GetRange(ctx, key, 10, -1)
		if err != nil {
			t.Fatalf("GetRange to-end: %v", err)
		}
		got, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read to-end range: %v", err)
		}
		if want := body[10:]; !bytes.Equal(got, want) {
			t.Fatalf("GetRange(10,-1) = %q; want %q", got, want)
		}
		// Missing key surfaces ErrNotFound like Get does.
		rc, err = store.GetRange(ctx, "org-1/run-5/nope", 0, 4)
		if rc != nil {
			rc.Close()
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetRange(missing) err = %v; want ErrNotFound", err)
		}
	})

	t.Run("stat", func(t *testing.T) {
		key := "org-1/run-6/stat.bin"
		body := []byte("twelve bytes")
		if err := store.Put(ctx, key, bytes.NewReader(body)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		info, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != int64(len(body)) {
			t.Fatalf("Stat size = %d; want %d", info.Size, len(body))
		}
		if _, err := store.Stat(ctx, "org-1/run-6/absent"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat(missing) err = %v; want ErrNotFound", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		// A dedicated prefix so the listing assertion is independent of the
		// other subtests' keys.
		prefix := "list-org/proj-9/kb/"
		want := map[string]int64{
			prefix + "a.md":         5,
			prefix + "b.txt":        3,
			prefix + "sub/deep.bin": 7,
		}
		for k, n := range want {
			if err := store.Put(ctx, k, bytes.NewReader(bytes.Repeat([]byte("x"), int(n)))); err != nil {
				t.Fatalf("Put %q: %v", k, err)
			}
		}
		got, err := store.List(ctx, prefix)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		gotByKey := map[string]int64{}
		for _, o := range got {
			gotByKey[o.Key] = o.Size
		}
		for k, n := range want {
			if gotByKey[k] != n {
				t.Fatalf("List missing/wrong %q: got size %d, want %d (full=%v)", k, gotByKey[k], n, gotByKey)
			}
		}
		// A prefix matching nothing is an empty listing, not an error.
		none, err := store.List(ctx, "list-org/does-not-exist/")
		if err != nil {
			t.Fatalf("List(empty prefix): %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("List(empty prefix) = %d entries; want 0", len(none))
		}
	})

	t.Run("large streamed blob", func(t *testing.T) {
		// 24 MiB is deliberately over the transfer manager's 16 MiB multipart
		// threshold (8 MiB parts), so Put streams as a real multipart upload —
		// CreateMultipartUpload / UploadPart×3 / CompleteMultipartUpload. That
		// path (not a single PutObject) is the part an S3-compatible backend is
		// most likely to get wrong, so the conformance suite must cross it.
		const size = 24 << 20 // 24 MiB
		key := "org-1/run-4/workspace.tar"
		if err := store.Put(ctx, key, &patternReader{n: size}); err != nil {
			t.Fatalf("Put large: %v", err)
		}
		rc, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get large: %v", err)
		}
		defer rc.Close()
		gotSum, gotN := streamSHA256(t, rc)
		wantSum, wantN := streamSHA256(t, &patternReader{n: size})
		if gotN != wantN {
			t.Fatalf("large blob length = %d; want %d", gotN, wantN)
		}
		if gotSum != wantSum {
			t.Fatalf("large blob sha256 = %s; want %s", gotSum, wantSum)
		}
	})
}

func mustGet(t *testing.T, store Storage, key string) []byte {
	t.Helper()
	rc, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return b
}

// streamSHA256 hashes r without holding it in memory, returning the hex
// digest and the byte count — so the large-blob check verifies a multi-
// megabyte body end to end without ever buffering a second copy.
func streamSHA256(t *testing.T, r io.Reader) (string, int64) {
	t.Helper()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		t.Fatalf("hash stream: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

// patternReader emits n bytes of a deterministic, position-derived pattern
// without allocating them up front, so the conformance suite can stream an
// arbitrarily large blob through Put. Two readers built with the same n
// emit identical bytes, which is how the large-blob check derives its
// expected digest without a golden file.
type patternReader struct {
	n int64
	i int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.i >= p.n {
		return 0, io.EOF
	}
	k := 0
	for k < len(b) && p.i < p.n {
		b[k] = byte(p.i)
		k++
		p.i++
	}
	return k, nil
}
