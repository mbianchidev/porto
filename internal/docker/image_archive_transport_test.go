package docker

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

func TestImageArchiveTextTransportPreservesBinaryBytesAcrossCRLF(t *testing.T) {
	input := bytes.Repeat([]byte{0, 10, 13, 255, 128, 42}, 10000)
	encoded := base64.StdEncoding.EncodeToString(input)
	var wrapped strings.Builder
	for len(encoded) > 0 {
		count := min(len(encoded), 76)
		wrapped.WriteString(encoded[:count])
		wrapped.WriteString("\r\n")
		encoded = encoded[count:]
	}
	output, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(wrapped.String())))
	if err != nil || !bytes.Equal(input, output) {
		t.Fatalf("binary image archive changed across the Windows transport: %v", err)
	}
}
