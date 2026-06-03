package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// minioImage pins the MinIO server the object-backend conformance test
// runs against. Pinned (not :latest) so a server-side behavior change
// can't silently alter what the suite asserts — same rationale as
// pgtest.Image.
const minioImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z"

func TestObjectStorage_Conformance(t *testing.T) {
	endpoint, accessKey, secretKey := startMinIO(t)
	cfg := ObjectConfig{
		Endpoint:  endpoint,
		Bucket:    "tf-conformance",
		AccessKey: accessKey,
		SecretKey: secretKey,
		Region:    "us-east-1",
	}
	store, err := newObjectStorage(cfg)
	if err != nil {
		t.Fatalf("newObjectStorage: %v", err)
	}
	ctx := context.Background()
	if _, err := store.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		// A re-run against a reused daemon may already own the bucket.
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			t.Fatalf("create bucket: %v", err)
		}
	}
	runConformance(t, store)
}

// TestObjectStorage_PreservesBasePath is the regression guard for the whole
// reason this backend uses aws-sdk: a path-prefixed endpoint (Supabase
// Storage's /storage/v1/s3) must reach the wire intact. It needs no Docker —
// a stub HTTP server records the request path. (A host-only S3 client would
// drop the prefix here.)
func TestObjectStorage_PreservesBasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("ETag", `"stub"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, err := newObjectStorage(ObjectConfig{
		Endpoint:  srv.URL + "/storage/v1/s3",
		Bucket:    "wb",
		AccessKey: "ak",
		SecretKey: "sk",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("newObjectStorage: %v", err)
	}
	if err := store.Put(context.Background(), "org/obj", strings.NewReader("hi")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "/storage/v1/s3/wb/org/obj"; gotPath != want {
		t.Fatalf("request path = %q; want %q (base path must reach the wire)", gotPath, want)
	}
}

// startMinIO boots a throwaway MinIO and returns its endpoint URL + root
// credentials. Like pgtest, it skips cleanly when Docker is unavailable —
// the SQLite/local suite stays green on machines with no daemon — but a
// reachable-but-broken daemon fails loudly rather than skipping a real
// regression into a green pass.
func startMinIO(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	const user, pass = "minioadmin", "minioadmin"
	req := testcontainers.ContainerRequest{
		Image:        minioImage,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"MINIO_ROOT_USER":     user,
			"MINIO_ROOT_PASSWORD": pass,
		},
		Cmd: []string{"server", "/data"},
		WaitingFor: wait.ForHTTP("/minio/health/ready").
			WithPort("9000/tcp").
			WithStartupTimeout(2 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start minio (Docker reachable but bring-up failed — not a skip): %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("minio host: %v", err)
	}
	port, err := c.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("minio port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port()), user, pass
}

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{in: "https://ref.supabase.co/storage/v1/s3", wantErr: false}, // base path is allowed and preserved
		{in: "http://localhost:9000", wantErr: false},
		{in: "https://s3.amazonaws.com", wantErr: false},
		{in: "minio:9000", wantErr: true},                   // scheme-less host-only form no longer accepted
		{in: "ftp://nope", wantErr: true},                   // unsupported scheme
		{in: "https://", wantErr: true},                     // no host
		{in: "http://minio:9000?debug=true", wantErr: true}, // query string rejected
		{in: "https://host/path#frag", wantErr: true},       // fragment rejected
	}
	for _, tc := range cases {
		if err := validateEndpoint(tc.in); (err != nil) != tc.wantErr {
			t.Errorf("validateEndpoint(%q) err = %v; wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestObjectConfigFromEnv(t *testing.T) {
	t.Run("requires endpoint", func(t *testing.T) {
		t.Setenv(envBlobBucket, "b")
		if _, err := ObjectConfigFromEnv(); err == nil {
			t.Fatal("ObjectConfigFromEnv with no endpoint = nil; want error")
		}
	})

	t.Run("requires bucket", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "http://minio:9000")
		if _, err := ObjectConfigFromEnv(); err == nil {
			t.Fatal("ObjectConfigFromEnv with no bucket = nil; want error")
		}
	})

	t.Run("preserves full endpoint URL including base path", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "https://ref.supabase.co/storage/v1/s3")
		t.Setenv(envBlobBucket, "workspaces")
		t.Setenv(envBlobAccessKey, "ak")
		t.Setenv(envBlobSecretKey, "sk")
		t.Setenv(envBlobRegion, "us-west-2")
		cfg, err := ObjectConfigFromEnv()
		if err != nil {
			t.Fatalf("ObjectConfigFromEnv: %v", err)
		}
		want := ObjectConfig{Endpoint: "https://ref.supabase.co/storage/v1/s3", Bucket: "workspaces", AccessKey: "ak", SecretKey: "sk", Region: "us-west-2"}
		if cfg != want {
			t.Fatalf("cfg = %+v; want %+v", cfg, want)
		}
	})

	t.Run("rejects scheme-less endpoint", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "minio:9000")
		t.Setenv(envBlobBucket, "b")
		if _, err := ObjectConfigFromEnv(); err == nil {
			t.Fatal("ObjectConfigFromEnv with scheme-less endpoint = nil; want error")
		}
	})
}

func TestNew_MultiRequiresBlobConfig(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envBlobEndpoint, "")
	t.Setenv(envBlobBucket, "")
	if _, err := New(); err == nil {
		t.Fatal("New in multi mode with no TF_BLOB_* = nil; want error (multi requires the object store)")
	}
}

func TestNew_MultiIsObject(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envBlobEndpoint, "http://minio:9000")
	t.Setenv(envBlobBucket, "blobs")
	t.Setenv(envBlobAccessKey, "ak")
	t.Setenv(envBlobSecretKey, "sk")
	store, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := store.(*objectStorage); !ok {
		t.Fatalf("New in multi mode = %T; want *objectStorage", store)
	}
}

func TestNewObjectStorage_NoCredsUsesDefaultChain(t *testing.T) {
	// With no static keys, construction goes through the AWS default
	// provider chain. It must still succeed offline (credentials resolve
	// lazily on first call, not at construction). HOME is pinned so the
	// shared-config loader can't trip over an unset home in the sandbox.
	t.Setenv("HOME", t.TempDir())
	store, err := newObjectStorage(ObjectConfig{Endpoint: "http://minio:9000", Bucket: "b", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("newObjectStorage (default chain): %v", err)
	}
	if store == nil {
		t.Fatal("newObjectStorage returned nil store")
	}
}

func TestNewObjectStorage_PartialCredsRejected(t *testing.T) {
	// Exactly one of access key / secret key set is a misconfiguration —
	// signing with an empty half fails every request with an opaque 403, so
	// it must be caught at construction, not deferred to the server.
	base := ObjectConfig{Endpoint: "http://minio:9000", Bucket: "b", Region: "us-east-1"}
	for _, tc := range []struct {
		name string
		cfg  ObjectConfig
	}{
		{"access only", func() ObjectConfig { c := base; c.AccessKey = "ak"; return c }()},
		{"secret only", func() ObjectConfig { c := base; c.SecretKey = "sk"; return c }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newObjectStorage(tc.cfg); err == nil {
				t.Fatalf("newObjectStorage(%s) = nil; want error for partial credentials", tc.name)
			}
		})
	}
}
