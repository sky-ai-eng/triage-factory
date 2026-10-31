package gitproxy

import "time"

// SetNowForTest replaces the Server's clock source so tests can age
// cached tokens past the refresh threshold without sleeping. Not
// exported in production builds (export_test.go is only compiled into
// the package's test binary).
func (s *Server) SetNowForTest(fn func() time.Time) {
	s.now = fn
}
