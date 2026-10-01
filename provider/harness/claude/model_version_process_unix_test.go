//go:build unix

package claude

import (
	"io"
	"strings"
	"testing"
)

func TestModelVersionPSOutputCopyBound(t *testing.T) {
	var output boundedModelVersionPSOutput
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", modelVersionPSOutputMax+1))}
	_, err := io.Copy(&output, source)
	if err == nil || output.Len() > modelVersionPSOutputMax {
		t.Fatalf("copy error=%v retained=%d; want refusal within %d bytes", err, output.Len(), modelVersionPSOutputMax)
	}
}
