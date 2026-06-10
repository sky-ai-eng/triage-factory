package llmproxy

import (
	"errors"
	"io"
	"net/http"
	"testing"
)

// closableBody simulates the server-owned request body: it serves data,
// then EOF — unless closed first, after which every Read fails with
// ErrBodyReadAfterClose exactly like net/http's server body does, even
// when the data had already been fully consumed.
type closableBody struct {
	data   []byte
	closed bool
}

func (b *closableBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		// EOF together with the final bytes, the shape http.body
		// produces for Content-Length'd requests.
		return n, io.EOF
	}
	return n, nil
}

func (b *closableBody) Close() error {
	b.closed = true
	return nil
}

// TestEOFLatchedBody_PostEOFReadsStayEOF pins the latch's reason for
// existing: after the body has been read to EOF, a server-side close
// (the handler's first response write readying the conn for reuse) must
// not turn later reads — the transport's over-length drain read — into
// ErrBodyReadAfterClose. That error fails the upstream Request.write and
// tears down the conn mid-stream.
func TestEOFLatchedBody_PostEOFReadsStayEOF(t *testing.T) {
	under := &closableBody{data: []byte(`{"stream":true}`)}
	body := &eofLatchedBody{rc: under}

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != `{"stream":true}` {
		t.Fatalf("ReadAll = %q, want the full body", got)
	}

	// The server closes the consumed body at first response flush.
	_ = under.Close()

	buf := make([]byte, 8)
	for i := 0; i < 2; i++ {
		n, err := body.Read(buf)
		if n != 0 || err != io.EOF {
			t.Fatalf("read %d after EOF+close = (%d, %v), want (0, io.EOF)", i, n, err)
		}
	}
}

// TestEOFLatchedBody_TruncationStillErrors pins the inverse boundary:
// the latch engages only after a genuine EOF. A body closed before full
// consumption was truncated — that must surface as the underlying error,
// not be papered over as a clean end.
func TestEOFLatchedBody_TruncationStillErrors(t *testing.T) {
	under := &closableBody{data: []byte(`{"stream":true}`)}
	body := &eofLatchedBody{rc: under}

	_ = under.Close() // close before any read: all data lost

	if _, err := body.Read(make([]byte, 8)); !errors.Is(err, http.ErrBodyReadAfterClose) {
		t.Fatalf("read after early close = %v, want ErrBodyReadAfterClose", err)
	}
}
