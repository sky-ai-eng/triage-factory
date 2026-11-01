package httpx

import "net/http"

// Mounts is how a handler subpackage registers its routes. API mounts a GET
// behind the session check; APIMutating mounts a POST, PUT, PATCH or DELETE
// behind the CSRF origin check and then the session check. A subpackage is
// handed these and never the mux, so it cannot mount a route the session does
// not guard.
type Mounts struct {
	API         func(pattern string, h http.HandlerFunc)
	APIMutating func(pattern string, h http.HandlerFunc)
}
