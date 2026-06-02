package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Env var names for the object backend. Documented in
// docs/self-host-setup.md and .env.example.
const (
	envBlobEndpoint  = "TF_BLOB_ENDPOINT"
	envBlobBucket    = "TF_BLOB_BUCKET"
	envBlobAccessKey = "TF_BLOB_ACCESS_KEY"
	envBlobSecretKey = "TF_BLOB_SECRET_KEY"
	envBlobRegion    = "TF_BLOB_REGION"
	envBlobUseSSL    = "TF_BLOB_USE_SSL"
)

// objectStorage is the multi-mode backend: an S3-compatible object store
// reached over the network, so a blob written by one executor is readable
// by the next to own the shard. minio-go is the client; any S3-protocol
// server (MinIO, AWS S3, Cloudflare R2, or Supabase Storage's S3 endpoint)
// sits behind it unchanged.
type objectStorage struct {
	client *minio.Client
	bucket string
}

// ObjectConfig is the S3 connection config for objectStorage, normally
// built from the environment by ObjectConfigFromEnv. Tests construct it
// directly to point at a throwaway MinIO container.
type ObjectConfig struct {
	Endpoint  string // host[:port], no scheme — the scheme drives UseSSL separately
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	Region    string // optional; S3 wants one, MinIO ignores it
}

// ObjectConfigFromEnv reads the TF_BLOB_* environment into an ObjectConfig.
// Endpoint and bucket are required; access key/secret are normally set but
// left optional so an IAM-role / instance-profile deployment can supply
// credentials out of band. TF_BLOB_ENDPOINT may carry a scheme
// (https://… ⇒ TLS, http://… ⇒ plaintext); a bare host[:port] defaults to
// TLS unless TF_BLOB_USE_SSL=false overrides it.
func ObjectConfigFromEnv() (ObjectConfig, error) {
	endpoint := strings.TrimSpace(os.Getenv(envBlobEndpoint))
	if endpoint == "" {
		return ObjectConfig{}, fmt.Errorf("%s is required in multi mode", envBlobEndpoint)
	}
	bucket := strings.TrimSpace(os.Getenv(envBlobBucket))
	if bucket == "" {
		return ObjectConfig{}, fmt.Errorf("%s is required in multi mode", envBlobBucket)
	}
	host, secure, err := parseEndpoint(endpoint)
	if err != nil {
		return ObjectConfig{}, err
	}
	if v := strings.TrimSpace(os.Getenv(envBlobUseSSL)); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return ObjectConfig{}, fmt.Errorf("%s=%q: %w", envBlobUseSSL, v, err)
		}
		secure = b
	}
	return ObjectConfig{
		Endpoint:  host,
		Bucket:    bucket,
		AccessKey: strings.TrimSpace(os.Getenv(envBlobAccessKey)),
		SecretKey: os.Getenv(envBlobSecretKey),
		UseSSL:    secure,
		Region:    strings.TrimSpace(os.Getenv(envBlobRegion)),
	}, nil
}

// parseEndpoint splits a TF_BLOB_ENDPOINT value into the bare host[:port]
// minio-go wants plus the TLS flag implied by any scheme. A value with a
// scheme (http/https) is parsed as a URL; a bare host[:port] defaults to
// TLS (the safe production default) and leaves the final say to
// TF_BLOB_USE_SSL.
func parseEndpoint(s string) (host string, secure bool, err error) {
	if !strings.Contains(s, "://") {
		return s, true, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false, fmt.Errorf("%s=%q: %w", envBlobEndpoint, s, err)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("%s=%q has no host", envBlobEndpoint, s)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("%s=%q: unsupported scheme %q (want http or https)", envBlobEndpoint, s, u.Scheme)
	}
}

// newObjectStorage builds the minio-go client for cfg. It does not create
// or probe the bucket — the bucket is provisioned by the deployment (the
// compose storage service / the operator) — and minio.New is lazy, so a
// bad endpoint surfaces on the first operation, not here.
func newObjectStorage(cfg ObjectConfig) (*objectStorage, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("storage: object endpoint is empty")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: object bucket is empty")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: init object client: %w", err)
	}
	return &objectStorage{client: client, bucket: cfg.Bucket}, nil
}

func (o *objectStorage) Put(ctx context.Context, key string, r io.Reader) error {
	// size -1 streams via multipart with a bounded part buffer, so a large
	// tarball never buffers whole in memory.
	if _, err := o.client.PutObject(ctx, o.bucket, key, r, -1, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	return nil
}

func (o *objectStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := o.client.GetObject(ctx, o.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	// GetObject is lazy: it returns a handle and performs the request only
	// on the first Read/Stat. Stat up front so a missing key surfaces as
	// ErrNotFound here — matching the filesystem backend — instead of on
	// the caller's first Read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	return obj, nil
}

func (o *objectStorage) Delete(ctx context.Context, key string) error {
	// S3 DELETE is idempotent — removing a missing key is a success — so
	// this matches the filesystem backend without a special case.
	if err := o.client.RemoveObject(ctx, o.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

func (o *objectStorage) Exists(ctx context.Context, key string) (bool, error) {
	if _, err := o.client.StatObject(ctx, o.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("storage: exists %q: %w", key, err)
	}
	return true, nil
}

// isNotFound reports whether an S3 error means "no such object". minio-go
// returns a typed ErrorResponse; a missing key is either the NoSuchKey
// code (GET) or a bare 404 (HEAD/StatObject, which carries no body to
// parse a code from).
func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}
