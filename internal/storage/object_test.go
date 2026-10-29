package storage

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seaweedImage pins the SeaweedFS server the object-backend conformance test
// runs against. Pinned (not :latest) so a server-side behavior change
// can't silently alter what the suite asserts — same rationale as
// pgtest.Image. Same tag the compose stack pins.
const seaweedImage = "chrislusf/seaweedfs:4.36"

func TestObjectStorage_Conformance(t *testing.T) {
	endpoint, accessKey, secretKey := startSeaweedFS(t)
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
	createBucket(t, store)
	runConformance(t, store)

	// The knowledge bucket is versioned, where a delete leaves a delete marker
	// and an overwrite keeps the old version. The backend must not tell the
	// difference: a deleted key is gone and an overwritten one reads new.
	versioned := cfg
	versioned.Bucket = "tf-conformance-versioned"
	vstore, err := newObjectStorage(versioned)
	if err != nil {
		t.Fatalf("newObjectStorage (versioned): %v", err)
	}
	createBucket(t, vstore)
	if _, err := vstore.client.PutBucketVersioning(context.Background(), &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(versioned.Bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	t.Run("versioned", func(t *testing.T) { runConformance(t, vstore) })
}

func createBucket(t *testing.T, store *objectStorage) {
	t.Helper()
	if _, err := store.client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(store.bucket)}); err != nil {
		// A re-run against a reused daemon may already own the bucket.
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			t.Fatalf("create bucket %s: %v", store.bucket, err)
		}
	}
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

// TestObjectStorage_TracesUnderParent checks that the S3 client sends through
// telemetry.TracedTransport: a Put made under a span records s3.http children
// of it, and the object key appears in none of their attributes, because a
// knowledge key carries team-authored folder and file names. It runs once per
// credential branch of newObjectStorage, since the default chain resolves the
// SDK's HTTP client through LoadDefaultConfig and static keys do not.
//
// It also checks the property SigV4 depends on with tracing on: traceparent
// reaches the wire but is not one of the headers the request was signed over.
// A verifier recomputes the signature from SignedHeaders only, so a header
// added after signing cannot invalidate it. TestObjectStorage_Conformance
// cannot show this, because its SeaweedFS runs in allow-all mode and ignores
// signatures.
//
// It replaces the global tracer provider and propagator, so it must not run in
// parallel with another test that reads them.
func TestObjectStorage_TracesUnderParent(t *testing.T) {
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	for _, tc := range []struct {
		name       string
		staticKeys bool
	}{
		{name: "static keys", staticKeys: true},
		{name: "default credential chain", staticKeys: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ObjectConfig{Bucket: "kb", Region: "us-east-1"}
			if tc.staticKeys {
				cfg.AccessKey, cfg.SecretKey = "ak", "sk"
			} else {
				// The env provider answers first. The config files point at
				// nothing, so a developer's own AWS profile can't change the
				// outcome, and AWS_CA_BUNDLE is always set, so the client
				// LoadDefaultConfig builds carries a root pool, the case
				// tracedHTTPClient has to preserve.
				dir := t.TempDir()
				none := filepath.Join(dir, "none")
				bundle := filepath.Join(dir, "ca.pem")
				ca := httptest.NewTLSServer(http.NotFoundHandler())
				ca.Close()
				if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate().Raw}), 0o600); err != nil {
					t.Fatalf("write CA bundle: %v", err)
				}
				for k, v := range map[string]string{
					"AWS_ACCESS_KEY_ID":           "ak",
					"AWS_SECRET_ACCESS_KEY":       "sk",
					"AWS_SESSION_TOKEN":           "",
					"AWS_PROFILE":                 "",
					"AWS_CONFIG_FILE":             none,
					"AWS_SHARED_CREDENTIALS_FILE": none,
					"AWS_CA_BUNDLE":               bundle,
				} {
					t.Setenv(k, v)
				}
			}

			recorder := tracetest.NewSpanRecorder()
			otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
			otel.SetTextMapPropagator(propagation.TraceContext{})

			type sent struct{ authorization, traceparent string }
			var (
				mu       sync.Mutex
				requests []sent
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, sent{r.Header.Get("Authorization"), r.Header.Get("traceparent")})
				mu.Unlock()
				w.Header().Set("ETag", `"stub"`)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			cfg.Endpoint = srv.URL

			// Built after the provider is installed: otelhttp resolves its
			// tracer and propagator when the transport is constructed.
			store, err := newObjectStorage(cfg)
			if err != nil {
				t.Fatalf("newObjectStorage: %v", err)
			}

			ctx, parent := otel.Tracer("test").Start(context.Background(), "workspace.snapshot.put")
			putErr := store.Put(ctx, "org/teams/t1/kb/private/secret-folder/notes.md", strings.NewReader("hi"))
			parent.End()
			if putErr != nil {
				t.Fatalf("Put: %v", putErr)
			}

			children := 0
			for _, span := range recorder.Ended() {
				if span.Name() != "s3.http" {
					continue
				}
				children++
				if span.Parent().SpanID() != parent.SpanContext().SpanID() {
					t.Errorf("s3.http parent = %s, want the calling span %s", span.Parent().SpanID(), parent.SpanContext().SpanID())
				}
				for _, kv := range span.Attributes() {
					if kv.Key == "url.full" || kv.Key == "url.path" {
						t.Errorf("s3.http carries %s (%q)", kv.Key, kv.Value.String())
					}
					if strings.Contains(kv.Value.String(), "secret-folder") {
						t.Errorf("attribute %s carries the object key (%q)", kv.Key, kv.Value.String())
					}
				}
			}
			if children == 0 {
				t.Fatal("Put recorded no s3.http span")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(requests) == 0 {
				t.Fatal("the stub received no request")
			}
			for i, req := range requests {
				if req.traceparent == "" {
					t.Errorf("request %d carried no traceparent", i)
				}
				_, rest, ok := strings.Cut(req.authorization, "SignedHeaders=")
				if !ok {
					t.Fatalf("request %d Authorization has no SignedHeaders: %q", i, req.authorization)
				}
				signed, _, _ := strings.Cut(rest, ",")
				for _, h := range strings.Split(signed, ";") {
					if h == "traceparent" || h == "tracestate" || h == "baggage" {
						t.Errorf("request %d was signed over %s; otelhttp sets it after signing, so the signature would not verify", i, h)
					}
				}
			}
		})
	}
}

// startSeaweedFS boots a throwaway SeaweedFS and returns its S3 endpoint URL
// plus a credential pair. It runs `weed mini` — the all-in-one single-process
// cluster (master+volume+filer+S3) built for fast startup — with no identity
// config, so the S3 gateway is in allow-all mode and the returned keys are
// accepted but unchecked (the conformance suite tests round-trip behavior, not
// auth enforcement; the compose stack runs with an s3.json identity instead).
// Like pgtest, it skips cleanly when Docker is unavailable — the SQLite/local
// suite stays green on machines with no daemon — but a reachable-but-broken
// daemon fails loudly rather than skipping a real regression into a green pass.
func startSeaweedFS(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	// Any non-empty pair works: allow-all mode ignores the SigV4 signature, and
	// newObjectStorage only requires that both halves are set.
	const user, pass = "tfaccesskey", "tfsecretkey"
	req := testcontainers.ContainerRequest{
		Image:        seaweedImage,
		ExposedPorts: []string{"8333/tcp"},
		Cmd:          []string{"mini", "-dir=/data"},
		// Gate on the S3 gateway itself answering on 8333 (the master/filer
		// come up first): any non-5xx status means it's serving — ListBuckets
		// returns 200 in allow-all mode.
		WaitingFor: wait.ForHTTP("/").
			WithPort("8333/tcp").
			WithStatusCodeMatcher(func(status int) bool { return status >= 200 && status < 500 }).
			WithStartupTimeout(2 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start seaweedfs (Docker reachable but bring-up failed — not a skip): %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("seaweedfs host: %v", err)
	}
	port, err := c.MappedPort(ctx, "8333/tcp")
	if err != nil {
		t.Fatalf("seaweedfs port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port()), user, pass
}

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{in: "https://ref.supabase.co/storage/v1/s3", wantErr: false}, // base path is allowed and preserved
		{in: "http://localhost:8333", wantErr: false},
		{in: "https://s3.amazonaws.com", wantErr: false},
		{in: "seaweedfs:8333", wantErr: true},                   // scheme-less host-only form no longer accepted
		{in: "ftp://nope", wantErr: true},                       // unsupported scheme
		{in: "https://", wantErr: true},                         // no host
		{in: "http://seaweedfs:8333?debug=true", wantErr: true}, // query string rejected
		{in: "https://host/path#frag", wantErr: true},           // fragment rejected
	}
	for _, tc := range cases {
		if err := validateEndpoint(tc.in); (err != nil) != tc.wantErr {
			t.Errorf("validateEndpoint(%q) err = %v; wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestObjectConfigFromEnv(t *testing.T) {
	t.Run("requires endpoint", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "")
		_, err := ObjectConfigFromEnv(WorkspaceBucket)
		if err == nil || !strings.Contains(err.Error(), envBlobEndpoint) {
			t.Fatalf("ObjectConfigFromEnv with no endpoint = %v; want an error naming %s", err, envBlobEndpoint)
		}
	})

	t.Run("bucket names default per bucket", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "http://seaweedfs:8333")
		t.Setenv("TF_WORKSPACE_BUCKET", "")
		t.Setenv("TF_KNOWLEDGE_BUCKET", "")
		for b, want := range map[Bucket]string{WorkspaceBucket: "tf-workspaces", KnowledgeBucket: "tf-knowledge"} {
			cfg, err := ObjectConfigFromEnv(b)
			if err != nil {
				t.Fatalf("ObjectConfigFromEnv(%s): %v", b.prefix, err)
			}
			if cfg.Bucket != want {
				t.Errorf("ObjectConfigFromEnv(%s).Bucket = %q; want %q", b.prefix, cfg.Bucket, want)
			}
		}
	})

	t.Run("each bucket reads its own variables and shares the endpoint", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "https://ref.supabase.co/storage/v1/s3")
		t.Setenv(envBlobRegion, "us-west-2")
		t.Setenv("TF_WORKSPACE_BUCKET", "ws")
		t.Setenv("TF_WORKSPACE_ACCESS_KEY", "ws-ak")
		t.Setenv("TF_WORKSPACE_SECRET_KEY", "ws-sk")
		t.Setenv("TF_KNOWLEDGE_BUCKET", "kb")
		t.Setenv("TF_KNOWLEDGE_ACCESS_KEY", "kb-ak")
		t.Setenv("TF_KNOWLEDGE_SECRET_KEY", "kb-sk")
		for b, want := range map[Bucket]ObjectConfig{
			WorkspaceBucket: {Endpoint: "https://ref.supabase.co/storage/v1/s3", Bucket: "ws", AccessKey: "ws-ak", SecretKey: "ws-sk", Region: "us-west-2"},
			KnowledgeBucket: {Endpoint: "https://ref.supabase.co/storage/v1/s3", Bucket: "kb", AccessKey: "kb-ak", SecretKey: "kb-sk", Region: "us-west-2"},
		} {
			cfg, err := ObjectConfigFromEnv(b)
			if err != nil {
				t.Fatalf("ObjectConfigFromEnv(%s): %v", b.prefix, err)
			}
			if cfg != want {
				t.Errorf("ObjectConfigFromEnv(%s) = %+v; want %+v", b.prefix, cfg, want)
			}
		}
	})

	t.Run("no keys is the AWS default chain", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "http://seaweedfs:8333")
		t.Setenv("TF_KNOWLEDGE_ACCESS_KEY", "")
		t.Setenv("TF_KNOWLEDGE_SECRET_KEY", "")
		cfg, err := ObjectConfigFromEnv(KnowledgeBucket)
		if err != nil {
			t.Fatalf("ObjectConfigFromEnv with no keys: %v", err)
		}
		if cfg.AccessKey != "" || cfg.SecretKey != "" {
			t.Fatalf("cfg keys = %q/%q; want both empty", cfg.AccessKey, cfg.SecretKey)
		}
	})

	t.Run("half a key pair names both variables", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "http://seaweedfs:8333")
		t.Setenv("TF_KNOWLEDGE_ACCESS_KEY", "ak")
		t.Setenv("TF_KNOWLEDGE_SECRET_KEY", "")
		_, err := ObjectConfigFromEnv(KnowledgeBucket)
		if err == nil || !strings.Contains(err.Error(), "TF_KNOWLEDGE_ACCESS_KEY") || !strings.Contains(err.Error(), "TF_KNOWLEDGE_SECRET_KEY") {
			t.Fatalf("ObjectConfigFromEnv with half a pair = %v; want an error naming both key variables", err)
		}
	})

	t.Run("rejects scheme-less endpoint", func(t *testing.T) {
		t.Setenv(envBlobEndpoint, "seaweedfs:8333")
		_, err := ObjectConfigFromEnv(WorkspaceBucket)
		if err == nil || !strings.Contains(err.Error(), envBlobEndpoint) {
			t.Fatalf("ObjectConfigFromEnv with scheme-less endpoint = %v; want an error naming %s", err, envBlobEndpoint)
		}
	})
}

func TestNewWorkspace_MultiRequiresEndpoint(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envBlobEndpoint, "")
	if _, err := NewWorkspace(); err == nil {
		t.Fatal("NewWorkspace in multi mode with no TF_BLOB_ENDPOINT = nil; want error (multi requires the object store)")
	}
}

func TestNewWorkspace_MultiIsObject(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envBlobEndpoint, "http://seaweedfs:8333")
	t.Setenv("TF_WORKSPACE_BUCKET", "")
	t.Setenv("TF_KNOWLEDGE_BUCKET", "")
	t.Setenv("TF_WORKSPACE_ACCESS_KEY", "ak")
	t.Setenv("TF_WORKSPACE_SECRET_KEY", "sk")
	store, err := NewWorkspace()
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	obj, ok := store.(*objectStorage)
	if !ok {
		t.Fatalf("NewWorkspace in multi mode = %T; want *objectStorage", store)
	}
	if obj.bucket != "tf-workspaces" {
		t.Fatalf("NewWorkspace bucket = %q; want tf-workspaces", obj.bucket)
	}
}

// TestNewWorkspace_RefusesTheKnowledgeBucket: one bucket for both would put
// snapshot overwrites under the knowledge base's versioning, and the two key
// pairs would scope nothing. The refusal names both variables, whichever of
// them was set — a workspace bucket named after the knowledge default is the
// same mistake.
func TestNewWorkspace_RefusesTheKnowledgeBucket(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envBlobEndpoint, "http://seaweedfs:8333")
	for _, tc := range []struct{ name, workspace, knowledge string }{
		{"both set", "shared", "shared"},
		{"knowledge default", "tf-knowledge", ""},
		{"workspace default", "", "tf-workspaces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TF_WORKSPACE_BUCKET", tc.workspace)
			t.Setenv("TF_KNOWLEDGE_BUCKET", tc.knowledge)
			_, err := NewWorkspace()
			if err == nil || !strings.Contains(err.Error(), "TF_WORKSPACE_BUCKET") || !strings.Contains(err.Error(), "TF_KNOWLEDGE_BUCKET") {
				t.Fatalf("NewWorkspace with one bucket for both = %v; want an error naming TF_WORKSPACE_BUCKET and TF_KNOWLEDGE_BUCKET", err)
			}
		})
	}
}

// TestNewObject_KnowledgeNeedsNoWorkspaceConfig: a control pod is given the
// knowledge variables and no workspace ones, so building the knowledge store
// must not read them.
func TestNewObject_KnowledgeNeedsNoWorkspaceConfig(t *testing.T) {
	t.Setenv(envBlobEndpoint, "http://seaweedfs:8333")
	t.Setenv("TF_KNOWLEDGE_BUCKET", "")
	t.Setenv("TF_KNOWLEDGE_ACCESS_KEY", "ak")
	t.Setenv("TF_KNOWLEDGE_SECRET_KEY", "sk")
	// Half a pair, which the workspace bucket would refuse if it were read.
	t.Setenv("TF_WORKSPACE_ACCESS_KEY", "ak")
	t.Setenv("TF_WORKSPACE_SECRET_KEY", "")
	store, err := NewObject(KnowledgeBucket)
	if err != nil {
		t.Fatalf("NewObject(KnowledgeBucket): %v", err)
	}
	if obj := store.(*objectStorage); obj.bucket != "tf-knowledge" {
		t.Fatalf("NewObject(KnowledgeBucket) bucket = %q; want tf-knowledge", obj.bucket)
	}
}

func TestNewObjectStorage_NoCredsUsesDefaultChain(t *testing.T) {
	// With no static keys, construction goes through the AWS default
	// provider chain. It must still succeed offline (credentials resolve
	// lazily on first call, not at construction). HOME is pinned so the
	// shared-config loader can't trip over an unset home in the sandbox.
	t.Setenv("HOME", t.TempDir())
	store, err := newObjectStorage(ObjectConfig{Endpoint: "http://seaweedfs:8333", Bucket: "b", Region: "us-east-1"})
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
	base := ObjectConfig{Endpoint: "http://seaweedfs:8333", Bucket: "b", Region: "us-east-1"}
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
