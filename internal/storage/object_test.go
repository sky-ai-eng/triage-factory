package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
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
		UseSSL:    false,
		Region:    "us-east-1",
	}
	store, err := newObjectStorage(cfg)
	if err != nil {
		t.Fatalf("newObjectStorage: %v", err)
	}
	ctx := context.Background()
	if err := store.client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
		// A re-run against a reused daemon may already own the bucket.
		exists, existsErr := store.client.BucketExists(ctx, cfg.Bucket)
		if existsErr != nil || !exists {
			t.Fatalf("make bucket: %v", err)
		}
	}
	runConformance(t, store)
}

// startMinIO boots a throwaway MinIO and returns its endpoint + root
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
	return fmt.Sprintf("%s:%s", host, port.Port()), user, pass
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		in         string
		wantHost   string
		wantSecure bool
		wantErr    bool
	}{
		{in: "minio:9000", wantHost: "minio:9000", wantSecure: true},
		{in: "https://s3.amazonaws.com", wantHost: "s3.amazonaws.com", wantSecure: true},
		{in: "http://localhost:9000", wantHost: "localhost:9000", wantSecure: false},
		{in: "ftp://nope", wantErr: true},
		{in: "https://", wantErr: true},
	}
	for _, tc := range cases {
		host, secure, err := parseEndpoint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEndpoint(%q) err = nil; want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEndpoint(%q) err = %v; want nil", tc.in, err)
			continue
		}
		if host != tc.wantHost || secure != tc.wantSecure {
			t.Errorf("parseEndpoint(%q) = %q, %v; want %q, %v", tc.in, host, secure, tc.wantHost, tc.wantSecure)
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

	t.Run("full config + scheme drives TLS", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "http://minio:9000")
		t.Setenv(envBlobBucket, "workspaces")
		t.Setenv(envBlobAccessKey, "ak")
		t.Setenv(envBlobSecretKey, "sk")
		t.Setenv(envBlobRegion, "us-west-2")
		cfg, err := ObjectConfigFromEnv()
		if err != nil {
			t.Fatalf("ObjectConfigFromEnv: %v", err)
		}
		want := ObjectConfig{Endpoint: "minio:9000", Bucket: "workspaces", AccessKey: "ak", SecretKey: "sk", UseSSL: false, Region: "us-west-2"}
		if cfg != want {
			t.Fatalf("cfg = %+v; want %+v", cfg, want)
		}
	})

	t.Run("use-ssl override wins over bare host default", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "minio:9000") // bare host defaults to TLS
		t.Setenv(envBlobBucket, "b")
		t.Setenv(envBlobUseSSL, "false")
		cfg, err := ObjectConfigFromEnv()
		if err != nil {
			t.Fatalf("ObjectConfigFromEnv: %v", err)
		}
		if cfg.UseSSL {
			t.Fatalf("UseSSL = true; want false (TF_BLOB_USE_SSL=false override)")
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
	t.Setenv(envBlobEndpoint, "http://minio:9000") // minio.New is lazy, so no server is contacted
	t.Setenv(envBlobBucket, "blobs")
	store, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := store.(*objectStorage); !ok {
		t.Fatalf("New in multi mode = %T; want *objectStorage", store)
	}
}
