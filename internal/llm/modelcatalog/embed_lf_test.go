package modelcatalog

import (
	"bytes"
	"testing"
)

// Embedded text must be byte-identical on every platform; a CR means a checkout converted line endings.
func TestEmbeddedText_NoCR(t *testing.T) {
	t.Parallel()
	embedded := map[string][]byte{
		"supported_models.json": supportedModelsJSON,
		"sdk_models.json":       sdkModelsJSON,
	}
	for name, b := range embedded {
		if bytes.IndexByte(b, '\r') >= 0 {
			t.Errorf("embedded %s contains a carriage return", name)
		}
	}
}
