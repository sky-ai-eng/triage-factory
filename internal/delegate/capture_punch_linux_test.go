//go:build linux

package delegate

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestPunchingReader_ReleasesWhatItHasRead: reading a stream file through the
// punching reader returns its consumed blocks to the filesystem, so the stream
// and its unpacked copy are not both on disk at the peak. The bytes it returns
// are unchanged.
func TestPunchingReader_ReleasesWhatItHasRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream")
	const size = 4 * punchEvery
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i%251 + 1)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	pr := newPunchingReader(f)
	var held int64
	buf := make([]byte, 1<<20)
	var got int64
	for {
		n, err := pr.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] != data[got+int64(i)] {
				t.Fatalf("byte %d differs", got+int64(i))
			}
		}
		got += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	held = st.Blocks * 512
	if !pr.canPunch {
		t.Skip("this filesystem cannot punch holes")
	}
	if got != size || st.Size != size {
		t.Fatalf("read %d bytes, size %d, want %d", got, st.Size, size)
	}
	if held > size/2 {
		t.Errorf("the file still holds %d of %d bytes after being read through", held, size)
	}
}
