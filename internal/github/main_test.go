package github

import (
	"os"
	"testing"
	"time"
)

// TestMain shrinks the transient backoff for the whole package: any test
// whose fake server answers a GET with a 5xx would otherwise spend 7s in
// real backoff before seeing the error it asserts on.
func TestMain(m *testing.M) {
	transientBackoffBase = time.Millisecond
	os.Exit(m.Run())
}
