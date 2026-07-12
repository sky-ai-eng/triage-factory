package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Env var names for the object backend. Documented in
// docs/self-hosting/storage.md and .env.example.
const (
	envBlobEndpoint  = "TF_BLOB_ENDPOINT"
	envBlobBucket    = "TF_BLOB_BUCKET"
	envBlobAccessKey = "TF_BLOB_ACCESS_KEY"
	envBlobSecretKey = "TF_BLOB_SECRET_KEY"
	envBlobRegion    = "TF_BLOB_REGION"
)

// objectStorage is the multi-mode backend: an S3-compatible object store
// reached over the network, so a blob written by one executor is readable
// by the next to own the shard. The client is aws-sdk-go-v2 rather than a
// host-only S3 client because its BaseEndpoint carries a full URL including
// a base path — which is what lets one client target a bare SeaweedFS/S3/R2
// endpoint AND Supabase Storage's path-prefixed S3 endpoint
// (https://<ref>.supabase.co/storage/v1/s3). Signing happens after endpoint
// resolution, so SigV4 covers the prefixed path the server verifies.
type objectStorage struct {
	client   *s3.Client
	uploader *transfermanager.Client
	bucket   string
}

// ObjectConfig is the S3 connection config for objectStorage, normally
// built from the environment by ObjectConfigFromEnv. Tests construct it
// directly to point at a throwaway SeaweedFS container.
type ObjectConfig struct {
	Endpoint  string // full URL: scheme://host[:port][/base/path]
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string // S3 wants one; SeaweedFS ignores it. Defaults to us-east-1.
}

// ObjectConfigFromEnv reads the TF_BLOB_* environment into an ObjectConfig.
// Endpoint and bucket are required; access key/secret are normally set but
// left optional so an instance-role / AWS-provider-chain deployment can
// supply credentials out of band (see newObjectStorage). TF_BLOB_ENDPOINT
// is a full URL — its scheme selects TLS and any base path is preserved.
func ObjectConfigFromEnv() (ObjectConfig, error) {
	endpoint := strings.TrimSpace(os.Getenv(envBlobEndpoint))
	if endpoint == "" {
		return ObjectConfig{}, fmt.Errorf("%s is required in multi mode", envBlobEndpoint)
	}
	if err := validateEndpoint(endpoint); err != nil {
		return ObjectConfig{}, err
	}
	bucket := strings.TrimSpace(os.Getenv(envBlobBucket))
	if bucket == "" {
		return ObjectConfig{}, fmt.Errorf("%s is required in multi mode", envBlobBucket)
	}
	return ObjectConfig{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: strings.TrimSpace(os.Getenv(envBlobAccessKey)),
		SecretKey: strings.TrimSpace(os.Getenv(envBlobSecretKey)),
		Region:    strings.TrimSpace(os.Getenv(envBlobRegion)),
	}, nil
}

// validateEndpoint checks TF_BLOB_ENDPOINT is a full URL aws-sdk's
// BaseEndpoint accepts: an http/https scheme plus a host, optionally with a
// base path (Supabase Storage's /storage/v1/s3). The path is deliberately
// kept — unlike a host-only S3 client, this backend carries it through to
// the wire.
func validateEndpoint(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%s=%q: %w", envBlobEndpoint, s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s=%q must be a full URL with an http or https scheme", envBlobEndpoint, s)
	}
	if u.Host == "" {
		return fmt.Errorf("%s=%q has no host", envBlobEndpoint, s)
	}
	// A base path is fine (Supabase's /storage/v1/s3), but a query or fragment
	// is meaningless for an S3 BaseEndpoint and would only confuse request
	// routing — reject it at boot rather than forwarding it silently.
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s=%q must not include a query string or fragment", envBlobEndpoint, s)
	}
	return nil
}

// newObjectStorage builds the aws-sdk S3 client for cfg. It does not create
// or probe the bucket — the bucket is provisioned by the deployment (the
// compose storage service / the operator). With explicit keys it signs with
// static V4; with neither key set it falls through to the standard AWS
// provider chain (AWS_* env, shared creds file, then EC2/ECS instance role)
// rather than signing with empty credentials. UsePathStyle is required by
// Supabase Storage and harmless for SeaweedFS/S3.
func newObjectStorage(cfg ObjectConfig) (*objectStorage, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("storage: object endpoint is empty")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: object bucket is empty")
	}
	region := cfg.Region
	if region == "" {
		// SeaweedFS ignores the region; S3/Supabase want one. Defaulting it
		// also keeps the default-chain branch below from probing IMDS for a
		// region at construction time.
		region = "us-east-1"
	}

	var awsCfg aws.Config
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		// Require both or neither. One-of-two would sign with an empty string
		// for the missing half and fail every request with an opaque 403; a
		// boot-time error is far easier to act on.
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("storage: %s and %s must both be set, or both omitted for the AWS default chain", envBlobAccessKey, envBlobSecretKey)
		}
		awsCfg = aws.Config{
			Region:      region,
			Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		}
	} else {
		loaded, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("storage: load aws config: %w", err)
		}
		awsCfg = loaded
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
	})
	return &objectStorage{
		client:   client,
		uploader: transfermanager.New(client),
		bucket:   cfg.Bucket,
	}, nil
}

func (o *objectStorage) Put(ctx context.Context, key string, r io.Reader) error {
	// The transfer manager streams via multipart with a bounded part buffer,
	// so a large tarball of unknown size never buffers whole in memory (plain
	// PutObject would need a seekable body or a content length). It delegates
	// to the s3 client above, so the BaseEndpoint/path-style config carries
	// through.
	if _, err := o.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
		Body:   r,
	}); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	return nil
}

func (o *objectStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := o.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	return resp.Body, nil
}

func (o *objectStorage) Delete(ctx context.Context, key string) error {
	// S3 DELETE is idempotent — removing a missing key is a success — so
	// this matches the filesystem backend without a special case.
	if _, err := o.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

func (o *objectStorage) Exists(ctx context.Context, key string) (bool, error) {
	if _, err := o.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("storage: exists %q: %w", key, err)
	}
	return true, nil
}

// isNotFound reports whether an S3 error means "no such object". GetObject
// surfaces a typed *types.NoSuchKey; HeadObject (a HEAD, no body to parse a
// code from) surfaces *types.NotFound or a bare 404, so we also accept a 404
// on the underlying HTTP response.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == http.StatusNotFound
	}
	return false
}
