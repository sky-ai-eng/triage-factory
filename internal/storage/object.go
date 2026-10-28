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
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"
)

// The deployment's object-store endpoint and region, shared by every bucket.
// Documented in docs/self-hosting/storage.md and .env.example.
const (
	envBlobEndpoint = "TF_BLOB_ENDPOINT"
	envBlobRegion   = "TF_BLOB_REGION"
)

// Bucket is one of the deployment's object-store buckets, configured by the
// variables its prefix names: <prefix>_BUCKET (defaulting to the bucket's
// name in the bundled stack), <prefix>_ACCESS_KEY and <prefix>_SECRET_KEY.
// Each bucket has its own key pair so a role can be given exactly the access
// it needs to each.
type Bucket struct {
	prefix      string
	defaultName string
}

var (
	// WorkspaceBucket holds workspace snapshots: a cache that the executors
	// alone read and write, and that a live run overwrites on every
	// checkpoint.
	WorkspaceBucket = Bucket{prefix: "TF_WORKSPACE", defaultName: "tf-workspaces"}
	// KnowledgeBucket holds team knowledge-base documents, the only copy of
	// what people wrote. Control pods write it; executors only read it.
	KnowledgeBucket = Bucket{prefix: "TF_KNOWLEDGE", defaultName: "tf-knowledge"}
)

func (b Bucket) bucketVar() string    { return b.prefix + "_BUCKET" }
func (b Bucket) accessKeyVar() string { return b.prefix + "_ACCESS_KEY" }
func (b Bucket) secretKeyVar() string { return b.prefix + "_SECRET_KEY" }

// name is the bucket's configured name, or its default when unset.
func (b Bucket) name() string {
	if v := strings.TrimSpace(os.Getenv(b.bucketVar())); v != "" {
		return v
	}
	return b.defaultName
}

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

// ObjectConfigFromEnv reads bucket b's configuration from the environment.
// TF_BLOB_ENDPOINT is required and is a full URL — its scheme selects TLS and
// any base path is preserved. The key pair is optional so an instance-role /
// AWS-provider-chain deployment can supply credentials out of band (see
// newObjectStorage), but it is both halves or neither.
func ObjectConfigFromEnv(b Bucket) (ObjectConfig, error) {
	endpoint := strings.TrimSpace(os.Getenv(envBlobEndpoint))
	if endpoint == "" {
		return ObjectConfig{}, fmt.Errorf("%s is required in multi mode", envBlobEndpoint)
	}
	if err := validateEndpoint(endpoint); err != nil {
		return ObjectConfig{}, err
	}
	cfg := ObjectConfig{
		Endpoint:  endpoint,
		Bucket:    b.name(),
		AccessKey: strings.TrimSpace(secretenv.Get(b.accessKeyVar())),
		SecretKey: strings.TrimSpace(secretenv.Get(b.secretKeyVar())),
		Region:    strings.TrimSpace(os.Getenv(envBlobRegion)),
	}
	// One of two would sign with an empty string for the missing half and
	// fail every request with an opaque 403; a boot-time error naming both
	// variables is far easier to act on.
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return ObjectConfig{}, fmt.Errorf("%s and %s must both be set, or both omitted for the AWS default chain", b.accessKeyVar(), b.secretKeyVar())
	}
	return cfg, nil
}

// NewObject builds the object store for bucket b from its environment. It
// does not create or probe the bucket; the deployment provisions it.
func NewObject(b Bucket) (Storage, error) {
	cfg, err := ObjectConfigFromEnv(b)
	if err != nil {
		return nil, err
	}
	return newObjectStorage(cfg)
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
		// Both or neither, as ObjectConfigFromEnv enforces for the env.
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("storage: access key and secret key must both be set, or both omitted for the AWS default chain")
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

func (o *objectStorage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	// A zero-length window would format an inverted "bytes=off-(off-1)" that
	// S3 rejects as InvalidRange. Nothing to fetch — return an empty body
	// rather than turn a degenerate request into a backend error.
	if length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	// The HTTP Range header the S3 GetObject API accepts: "bytes=off-" for
	// to-end, "bytes=off-last" for a bounded window (last is inclusive, so
	// off+length-1). The server responds 206 with just those bytes.
	var rangeHdr string
	if length < 0 {
		rangeHdr = fmt.Sprintf("bytes=%d-", offset)
	} else {
		rangeHdr = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	resp, err := o.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHdr),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: get range %q: %w", key, err)
	}
	return resp.Body, nil
}

func (o *objectStorage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	var token *string
	for {
		resp, err := o.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(o.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("storage: list %q: %w", prefix, err)
		}
		for _, obj := range resp.Contents {
			info := ObjectInfo{Key: aws.ToString(obj.Key), Size: aws.ToInt64(obj.Size)}
			if obj.LastModified != nil {
				info.ModTime = *obj.LastModified
			}
			out = append(out, info)
		}
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
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

func (o *objectStorage) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	resp, err := o.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("storage: stat %q: %w", key, err)
	}
	info := ObjectInfo{Key: key, Size: aws.ToInt64(resp.ContentLength)}
	if resp.LastModified != nil {
		info.ModTime = *resp.LastModified
	}
	return info, nil
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
