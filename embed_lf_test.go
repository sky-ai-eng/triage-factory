package main

import (
	"bytes"
	"io/fs"
	"testing"
)

// Embedded deployment files must be byte-identical on every platform; a CR means a checkout converted line endings.
func TestEmbeddedDeployBundle_NoCR(t *testing.T) {
	t.Parallel()
	files := 0
	err := fs.WalkDir(deployBundle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, err := fs.ReadFile(deployBundle, path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, '\r') >= 0 {
			t.Errorf("embedded %s contains a carriage return", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy bundle: %v", err)
	}
	if files == 0 {
		t.Fatal("no embedded deploy files found")
	}
}
