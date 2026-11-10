package memoryprovision

import (
	"bytes"
	"testing"
)

// Embedded text must be byte-identical on every platform; a CR means a checkout converted line endings.
func TestEmbeddedText_NoCR(t *testing.T) {
	t.Parallel()
	embedded := map[string][]byte{
		"generate-memory":        []byte(generateMemoryPrompt),
		"generate-memory-system": []byte(generateMemorySystemPrompt),
		"generate-memory-user":   []byte(generateMemoryUserPrompt),
	}
	for name, b := range embedded {
		if bytes.IndexByte(b, '\r') >= 0 {
			t.Errorf("embedded %s contains a carriage return", name)
		}
	}
}
