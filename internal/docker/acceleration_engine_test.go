package docker

import (
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/acceleration"
)

func TestChangingTranslatorPreservesExistingVZDevices(t *testing.T) {
	original := []byte("vmType: vz\nvmOpts:\n  vz:\n    rosetta:\n      enabled: true\n      binfmt: true\n")
	expression, err := accelerationEngineExpression(acceleration.Settings{Policy: acceleration.QEMUCompatible}, original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expression, ".vmOpts.vz.rosetta.enabled = true") || !strings.Contains(expression, ".vmOpts.vz.rosetta.binfmt = false") {
		t.Fatalf("QEMU policy changed existing VZ devices instead of only selecting its handler: %s", expression)
	}
}
