package db

import (
	"net/url"
	"strings"
	"testing"
)

func TestRewriteDSNCreds_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		user     string
		password string
		wantUser string
		wantPass string
	}{
		{
			name:     "basic url",
			dsn:      "postgres://postgres:secret@postgres:5432/postgres",
			user:     "authenticator",
			password: "newpw",
			wantUser: "authenticator",
			wantPass: "newpw",
		},
		{
			name:     "with options",
			dsn:      "postgres://postgres:secret@postgres:5432/postgres?sslmode=disable&search_path=auth",
			user:     "authenticator",
			password: "newpw",
			wantUser: "authenticator",
			wantPass: "newpw",
		},
		{
			name:     "no existing password",
			dsn:      "postgres://postgres@postgres:5432/postgres",
			user:     "authenticator",
			password: "newpw",
			wantUser: "authenticator",
			wantPass: "newpw",
		},
		{
			name:     "password contains url-reserved chars",
			dsn:      "postgres://postgres:secret@postgres:5432/postgres",
			user:     "authenticator",
			password: "p@ss/w?rd#",
			wantUser: "authenticator",
			wantPass: "p@ss/w?rd#",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RewriteDSNCreds(tc.dsn, tc.user, tc.password)
			if err != nil {
				t.Fatalf("RewriteDSNCreds: %v", err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			if u.User.Username() != tc.wantUser {
				t.Errorf("user = %q, want %q", u.User.Username(), tc.wantUser)
			}
			gotPass, _ := u.User.Password()
			if gotPass != tc.wantPass {
				t.Errorf("password = %q, want %q", gotPass, tc.wantPass)
			}
			// Host/path/query must round-trip unchanged.
			orig, _ := url.Parse(tc.dsn)
			if u.Host != orig.Host {
				t.Errorf("host mismatch: %q vs %q", u.Host, orig.Host)
			}
			if u.Path != orig.Path {
				t.Errorf("path mismatch: %q vs %q", u.Path, orig.Path)
			}
			if u.RawQuery != orig.RawQuery {
				t.Errorf("query mismatch: %q vs %q", u.RawQuery, orig.RawQuery)
			}
		})
	}
}

func TestRewriteDSNCreds_Errors(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"empty", ""},
		{"no scheme", "postgres:5432/postgres"},
		{"keyword form", "host=postgres user=postgres password=secret"},
		{"non-postgres scheme", "mysql://root:pw@host:3306/db"},
		{"http scheme", "http://host:5432/postgres"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RewriteDSNCreds(tc.dsn, "authenticator", "pw")
			if err == nil {
				t.Errorf("want error on dsn=%q, got nil", tc.dsn)
			} else if !strings.Contains(err.Error(), "dsn") && !strings.Contains(err.Error(), "parse") && !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error %q lacks expected context", err)
			}
		})
	}
}

func TestRewriteDSNCreds_EmptyUser(t *testing.T) {
	_, err := RewriteDSNCreds("postgres://postgres:secret@postgres:5432/postgres", "", "pw")
	if err == nil {
		t.Fatalf("want error on empty user, got nil")
	}
	if !strings.Contains(err.Error(), "user") {
		t.Errorf("error %q lacks expected context", err)
	}
}

func TestRewriteDSNCreds_PostgresqlScheme(t *testing.T) {
	// The `postgresql://` scheme is the long-form synonym for
	// `postgres://`; both should be accepted.
	got, err := RewriteDSNCreds("postgresql://postgres:secret@postgres:5432/postgres", "authenticator", "pw")
	if err != nil {
		t.Fatalf("RewriteDSNCreds: %v", err)
	}
	if !strings.HasPrefix(got, "postgresql://authenticator:pw@") {
		t.Errorf("unexpected output: %q", got)
	}
}

// TestDSN_TestMemoryMatchesProductionStorageParams pins the one property that
// makes the in-repo test DSN worth having: it selects the same on-disk
// timestamp layout OpenAt does.
//
// The suite spent months on the driver's default layout — Go's time.String() —
// while every shipped binary wrote the sqlite layout. Nothing failed, because
// both sides of every assertion agreed with each other; the tests were simply
// describing a database that did not exist. Two timestamp-comparison defects
// lived under that gap.
//
// Only the storage-affecting parameter is compared. The production DSN also
// carries WAL and busy_timeout, which are about a file on disk and mean
// nothing to :memory:.
func TestDSN_TestMemoryMatchesProductionStorageParams(t *testing.T) {
	if !strings.Contains(TestDSNMemory, SQLiteTimeFormatParam) {
		t.Fatalf("TestDSNMemory (%q) does not carry %q — tests would store a timestamp layout "+
			"production never writes", TestDSNMemory, SQLiteTimeFormatParam)
	}
	// The production spelling, reconstructed the way OpenAt builds it, so a
	// change to either side has to be made on both.
	prod := "/tmp/x.db" +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=busy_timeout(5000)" +
		"&" + SQLiteTimeFormatParam
	for _, param := range []string{"_pragma=foreign_keys(on)", SQLiteTimeFormatParam} {
		if !strings.Contains(prod, param) {
			t.Errorf("production DSN no longer carries %q; TestDSNMemory still does", param)
		}
		if !strings.Contains(TestDSNMemory, param) {
			t.Errorf("TestDSNMemory no longer carries %q; the production DSN still does", param)
		}
	}
}
