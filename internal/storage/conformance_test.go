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
