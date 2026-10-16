package sandbox

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// CaptureStreamFile is the one staging file a capture jail writes its members
// to, through a descriptor the broker opens and passes in. The orchestrator
// prepares it, the broker validates it, and the jail has no other writable
// path.
const CaptureStreamFile = "capture.stream"

// CaptureStreamFD is the descriptor number the capture jail finds the stream
// on.
const CaptureStreamFD = 3

const (
	// captureStreamChunk is the largest piece a member's bytes travel in.
	captureStreamChunk = 1 << 20
	// CaptureMemberNameMax bounds a member name.
	CaptureMemberNameMax = 1024
)

// CaptureStreamMaxBytes bounds everything a capture's stream may carry. The
// jail is TF's own code run over files the agent wrote, and its output is read
// as hostile all the same: without a bound a capture that went wrong could fill
// the executor's disk. It is enforced twice: runsc's file-size limit stops the
// writer at it, and the reader refuses a stream past it. A var so tests can
// shrink it, though not below what the sandbox itself needs: runsc's memory
// file grows to the jail's working memory under the same limit.
var CaptureStreamMaxBytes int64 = 8 << 30

// CaptureStreamWriter frames named members onto w. A member is its name, then
// its bytes in length-prefixed chunks, then a zero-length chunk. The framing
// needs no size ahead of the bytes, which is why a git bundle or a diff can be
// written straight from git into it without being spooled to the jail's
// memory-backed /tmp.
type CaptureStreamWriter struct {
	w   *bufio.Writer
	cur bool
}

// NewCaptureStreamWriter returns a writer framing onto w.
func NewCaptureStreamWriter(w io.Writer) *CaptureStreamWriter {
	return &CaptureStreamWriter{w: bufio.NewWriterSize(w, captureStreamChunk)}
}

// Member starts a member and returns the writer for its bytes. The previous
// member must have been closed.
func (s *CaptureStreamWriter) Member(name string) (io.WriteCloser, error) {
	if s.cur {
		return nil, errors.New("sandbox: capture stream: member opened before the last was closed")
	}
	if name == "" || len(name) > CaptureMemberNameMax {
		return nil, fmt.Errorf("sandbox: capture stream: member name length %d out of range", len(name))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(name)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return nil, err
	}
	if _, err := s.w.WriteString(name); err != nil {
		return nil, err
	}
	s.cur = true
	return &captureMemberWriter{s: s}, nil
}

// Close flushes the stream. It carries no terminator: the end of the file is
// the end of the stream, and a member cut short is an error to the reader.
func (s *CaptureStreamWriter) Close() error { return s.w.Flush() }

type captureMemberWriter struct {
	s      *CaptureStreamWriter
	closed bool
}

func (m *captureMemberWriter) Write(p []byte) (int, error) {
	if m.closed {
		return 0, errors.New("sandbox: capture stream: write to a closed member")
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), captureStreamChunk)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(n))
		if _, err := m.s.w.Write(hdr[:]); err != nil {
			return written, err
		}
		if _, err := m.s.w.Write(p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (m *captureMemberWriter) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	m.s.cur = false
	var hdr [4]byte
	_, err := m.s.w.Write(hdr[:])
	return err
}

// CaptureStreamReader reads the members a CaptureStreamWriter framed. Every
// length it reads came from a process that read agent-written files, so each is
// checked against a bound before it is believed.
type CaptureStreamReader struct {
	r    *bufio.Reader
	read int64
	cur  *captureMemberReader
}

// NewCaptureStreamReader returns a reader over r.
func NewCaptureStreamReader(r io.Reader) *CaptureStreamReader {
	return &CaptureStreamReader{r: bufio.NewReaderSize(r, captureStreamChunk)}
}

// Next advances to the next member and returns its name and a reader for its
// bytes, which must be drained or abandoned before Next is called again.
// io.EOF means the stream ended between members.
func (s *CaptureStreamReader) Next() (string, io.Reader, error) {
	if s.cur != nil {
		if _, err := io.Copy(io.Discard, s.cur); err != nil {
			return "", nil, err
		}
	}
	var hdr [2]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil, io.EOF
		}
		return "", nil, fmt.Errorf("sandbox: capture stream: read member header: %w", err)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 || n > CaptureMemberNameMax {
		return "", nil, fmt.Errorf("sandbox: capture stream: member name length %d out of range", n)
	}
	name := make([]byte, n)
	if _, err := io.ReadFull(s.r, name); err != nil {
		return "", nil, fmt.Errorf("sandbox: capture stream: read member name: %w", unexpectedEOF(err))
	}
	s.read += int64(2 + n)
	s.cur = &captureMemberReader{s: s}
	return string(name), s.cur, nil
}

type captureMemberReader struct {
	s    *CaptureStreamReader
	left int
	done bool
}

func (m *captureMemberReader) Read(p []byte) (int, error) {
	if m.done {
		return 0, io.EOF
	}
	s := m.s
	if m.left == 0 {
		var hdr [4]byte
		if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
			return 0, fmt.Errorf("sandbox: capture stream: read chunk header: %w", unexpectedEOF(err))
		}
		n := binary.BigEndian.Uint32(hdr[:])
		s.read += 4
		if n == 0 {
			m.done = true
			return 0, io.EOF
		}
		if n > captureStreamChunk {
			return 0, fmt.Errorf("sandbox: capture stream: chunk of %d bytes exceeds %d", n, captureStreamChunk)
		}
		if s.read+int64(n) > CaptureStreamMaxBytes {
			return 0, fmt.Errorf("sandbox: capture stream exceeds %d bytes", CaptureStreamMaxBytes)
		}
		m.left = int(n)
	}
	if len(p) > m.left {
		p = p[:m.left]
	}
	n, err := s.r.Read(p)
	m.left -= n
	s.read += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("sandbox: capture stream: %w", err)
	}
	if errors.Is(err, io.EOF) && m.left > 0 {
		return n, fmt.Errorf("sandbox: capture stream: %w", io.ErrUnexpectedEOF)
	}
	return n, nil
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
