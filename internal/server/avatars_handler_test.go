package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// tinyPNG returns a valid 1x1 PNG — the smallest real image the proxy will
// accept, used as the upstream avatar body in the fetch/handler tests.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

// tinyJPEG returns a valid 1x1 JPEG — a second allowed raster type, so the
// sniff-based content-type check is exercised across formats.
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// TestIsPublicUnicastIP pins the SSRF predicate shared by the avatar dial guard
// and the GitHub manifest reachability check: only globally-routable public
// unicast is allowed; loopback / private / link-local / CGNAT / doc / reserved
// are not.
func TestIsPublicUnicastIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"::ffff:8.8.8.8", true}, // v4-mapped public, unmapped before the check
		{"127.0.0.1", false},     // loopback
		{"::1", false},           // loopback
		{"0.0.0.0", false},       // unspecified
		{"10.0.0.5", false},      // RFC1918
		{"192.168.1.10", false},  // RFC1918
		{"172.16.4.2", false},    // RFC1918
		{"169.254.10.1", false},  // link-local
		{"fe80::1", false},       // link-local v6
		{"fd00::1", false},       // unique-local v6
		{"100.64.0.1", false},    // CGNAT
		{"192.0.2.5", false},     // TEST-NET-1
		{"240.0.0.1", false},     // reserved
		{"2001:db8::1", false},   // IPv6 documentation
		{"ff02::1", false},       // multicast
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ip, err)
		}
		if got := isPublicUnicastIP(ip); got != tc.want {
			t.Errorf("isPublicUnicastIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestSafeAvatarDialControl confirms the dial-time guard accepts a public
// resolved address and rejects loopback/private ones (the layer that defeats
// DNS rebinding, since it runs on the post-resolution address).
func TestSafeAvatarDialControl(t *testing.T) {
	cases := []struct {
		addr   string
		wantOK bool
	}{
		{"8.8.8.8:443", true},
		{"[2606:4700:4700::1111]:443", true},
		{"127.0.0.1:443", false},
		{"[::1]:443", false},
		{"10.0.0.1:443", false},
		{"169.254.1.1:80", false},
		{"not-an-ip:443", false}, // a hostname here means resolution didn't happen
		{"garbage", false},       // missing port
	}
	for _, tc := range cases {
		err := safeAvatarDialControl("tcp", tc.addr, nil)
		if (err == nil) != tc.wantOK {
			t.Errorf("safeAvatarDialControl(%q) err=%v, wantOK=%v", tc.addr, err, tc.wantOK)
		}
	}
}

// TestNormalizeImageType pins the content-type resolution: it SNIFFS the bytes
// and ignores any upstream header, so only data that really is an allowed raster
// image is served (the response carries a real image type under the global
// nosniff header, and a spoofed header can't smuggle non-image content).
func TestNormalizeImageType(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"sniffs png", tinyPNG(t), "image/png"},
		{"sniffs jpeg", tinyJPEG(t), "image/jpeg"},
		{"rejects html", []byte("<!doctype html><html></html>"), ""},
		{"rejects svg (not a raster image)", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), ""},
		{"rejects empty body", []byte{}, ""},
	}
	for _, tc := range cases {
		if got := normalizeImageType(tc.data); got != tc.want {
			t.Errorf("%s: normalizeImageType() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAvatarFetcher_FetchAndCache covers the fetcher's core contract without a
// DB: a first fetch hits the upstream and caches the bytes; a second is served
// from cache (no second upstream hit). Uses a TLS test server (https-only) with
// its own client, so the production SSRF dial guard (which would reject the
// loopback upstream) is bypassed — that guard is unit-tested separately.
func TestAvatarFetcher_FetchAndCache(t *testing.T) {
	want := tinyPNG(t)
	var hits int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(want)
	}))
	defer ts.Close()

	f := newAvatarFetcher()
	f.client = ts.Client()

	img, err := f.fetch(ts.URL)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if img.contentType != "image/png" || !bytes.Equal(img.data, want) {
		t.Fatalf("fetched %d bytes ct=%q, want %d bytes image/png", len(img.data), img.contentType, len(want))
	}
	if _, err := f.fetch(ts.URL); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (second fetch should be cached)", got)
	}
}

// TestAvatarFetcher_NegativeCache confirms an upstream failure is cached briefly
// so a broken avatar doesn't re-hammer the upstream every render.
func TestAvatarFetcher_NegativeCache(t *testing.T) {
	var hits int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	f := newAvatarFetcher()
	f.client = ts.Client()

	if _, err := f.fetch(ts.URL); err == nil {
		t.Fatal("first fetch: want error on upstream 500")
	}
	if _, err := f.fetch(ts.URL); err == nil {
		t.Fatal("second fetch: want cached error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (failure should be negative-cached)", got)
	}
}

// TestAvatarFetcher_RejectsOversize confirms the size cap fires on a body larger
// than maxAvatarBytes.
func TestAvatarFetcher_RejectsOversize(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, maxAvatarBytes+1))
	}))
	defer ts.Close()

	f := newAvatarFetcher()
	f.client = ts.Client()
	if _, err := f.fetch(ts.URL); err == nil {
		t.Fatal("want error on oversize avatar")
	}
}

// TestAvatarFetcher_RejectsNonHTTPS confirms the https-only guard rejects a
// plain-http avatar URL before any dial.
func TestAvatarFetcher_RejectsNonHTTPS(t *testing.T) {
	f := newAvatarFetcher()
	if _, err := f.doFetch(context.Background(), "http://avatars.example.com/u/1.png"); err == nil {
		t.Fatal("want error on non-https avatar url")
	}
}

// TestAvatarFetcher_BoundsCacheBytes confirms the total-bytes budget evicts so
// many distinct large images can't grow the cache without limit (the OOM risk a
// pure entry-count cap leaves open). Limits are shrunk for a cheap test.
func TestAvatarFetcher_BoundsCacheBytes(t *testing.T) {
	defer func(b int64, e int) { avatarCacheMaxBytes, avatarCacheMaxEntries = b, e }(avatarCacheMaxBytes, avatarCacheMaxEntries)
	avatarCacheMaxBytes = 8 * 1024 // 8 KiB budget
	avatarCacheMaxEntries = 1000   // high, so the byte budget is what binds here

	f := newAvatarFetcher()
	for i := 0; i < 64; i++ {
		f.cachePut(fmt.Sprintf("https://cdn/%d", i), &fetchedAvatar{data: make([]byte, 1024), contentType: "image/png"})
	}
	f.mu.Lock()
	total, n := f.totalBytes, len(f.cache)
	f.mu.Unlock()
	if total > avatarCacheMaxBytes {
		t.Errorf("totalBytes = %d, want <= %d (byte budget)", total, avatarCacheMaxBytes)
	}
	if n == 0 || n > 8 {
		t.Errorf("entries = %d, want a small nonzero count within the 8-image byte budget", n)
	}
}

// TestAvatarFetcher_BoundsNegativeEntries confirms the entry-count cap bounds a
// flood of distinct FAILED urls — negative entries hold no bytes, so the byte
// budget can't evict them; the count cap must.
func TestAvatarFetcher_BoundsNegativeEntries(t *testing.T) {
	defer func(e int) { avatarCacheMaxEntries = e }(avatarCacheMaxEntries)
	avatarCacheMaxEntries = 16

	f := newAvatarFetcher()
	for i := 0; i < 100; i++ {
		f.cachePut(fmt.Sprintf("https://cdn/fail/%d", i), nil) // negative cache, 0 bytes
	}
	f.mu.Lock()
	n, total := len(f.cache), f.totalBytes
	f.mu.Unlock()
	if n > avatarCacheMaxEntries {
		t.Errorf("entries = %d, want <= %d (entry-count cap)", n, avatarCacheMaxEntries)
	}
	if total != 0 {
		t.Errorf("totalBytes = %d, want 0 (negative entries hold no bytes)", total)
	}
}

// TestAvatarsHandler_Postgres pins the handler's authz + RLS scoping under real
// Postgres RLS: a member proxies their own and a co-member's avatar (200), a
// cross-org caller cannot (404 — the row is invisible, never a leak), and an
// absent avatar / malformed id 404 so the frontend falls back to a monogram.
// Skips without Docker via the pgtest harness (reused usage rig).
func TestAvatarsHandler_Postgres(t *testing.T) {
	r := newUsageRig(t)

	want := tinyPNG(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(want)
	}))
	defer ts.Close()

	// member has an avatar; owner deliberately does not (the monogram path).
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE public.users SET avatar_url = $1 WHERE id = $2`, ts.URL, r.member)

	// A second org + user, with no membership in r.orgID, for the cross-org case.
	otherOrg, otherUser, _ := pgtest.SeedOrgWithUser(t, r.h, "avatar-other-org")

	f := newAvatarFetcher()
	f.client = ts.Client()
	h := &avatarsHandler{tx: r.uh.tx, fetcher: f}

	avatarReq := func(orgID, caller, target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/avatars/"+target, nil)
		req.SetPathValue("user_id", target)
		ctx := httpx.WithOrgID(req.Context(), orgID)
		ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
		return req.WithContext(ctx)
	}

	t.Run("member_fetches_own_avatar", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.handleAvatar(rec, avatarReq(r.orgID, r.member, r.member))
		if rec.Code != http.StatusOK {
			t.Fatalf("own avatar = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("content-type = %q, want image/png", ct)
		}
		if !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("body = %d bytes, want the %d-byte upstream png", rec.Body.Len(), len(want))
		}
		if cc := rec.Header().Get("Cache-Control"); cc == "" {
			t.Errorf("missing Cache-Control header")
		}
	})

	t.Run("co_member_fetches_member_avatar", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.handleAvatar(rec, avatarReq(r.orgID, r.owner, r.member))
		if rec.Code != http.StatusOK {
			t.Fatalf("co-member avatar = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cross_org_caller_404", func(t *testing.T) {
		// otherUser is in otherOrg only; member's row is invisible under its RLS,
		// so the avatar resolves empty → 404 (non-disclosure), not the image.
		rec := httptest.NewRecorder()
		h.handleAvatar(rec, avatarReq(otherOrg, otherUser, r.member))
		if rec.Code != http.StatusNotFound {
			t.Errorf("cross-org avatar = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no_avatar_404", func(t *testing.T) {
		// owner has no avatar_url → 404 → frontend monogram.
		rec := httptest.NewRecorder()
		h.handleAvatar(rec, avatarReq(r.orgID, r.member, r.owner))
		if rec.Code != http.StatusNotFound {
			t.Errorf("no-avatar = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("malformed_id_404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.handleAvatar(rec, avatarReq(r.orgID, r.member, "not-a-uuid"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("malformed id = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})
}
