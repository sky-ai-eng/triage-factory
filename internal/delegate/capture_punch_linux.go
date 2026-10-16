//go:build linux

package delegate

import (
	"os"

	"golang.org/x/sys/unix"
)

// punchEvery is how much of the stream is read between releases of its
// consumed prefix.
const punchEvery = 32 << 20

// punchingReader reads a capture stream file and returns the disk blocks of
// what it has read to the filesystem as it goes. The unpacker copies every
// member into staging, so without this the stream and its copy are on disk
// together, twice the stream's size at the peak; with it the peak is the
// stream's size plus one interval. A filesystem that cannot punch holes keeps
// the blocks, which is the bound the stream's own cap already gives.
type punchingReader struct {
	f        *os.File
	read     int64
	punched  int64
	canPunch bool
}

func newPunchingReader(f *os.File) *punchingReader {
	return &punchingReader{f: f, canPunch: true}
}

func (p *punchingReader) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	p.read += int64(n)
	if p.canPunch && p.read-p.punched >= punchEvery {
		if perr := unix.Fallocate(int(p.f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, p.punched, p.read-p.punched); perr != nil {
			p.canPunch = false
		} else {
			p.punched = p.read
		}
	}
	return n, err
}
