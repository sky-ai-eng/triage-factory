//go:build !linux

package delegate

import "os"

// punchingReader is a plain reader where the stream's consumed blocks cannot be
// handed back; the stream's own cap bounds the disk it holds.
type punchingReader struct{ f *os.File }

func newPunchingReader(f *os.File) *punchingReader { return &punchingReader{f: f} }

func (p *punchingReader) Read(b []byte) (int, error) { return p.f.Read(b) }
