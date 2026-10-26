package acceleration

import "testing"

func TestDarwinFrameworkCapabilityProbeIsReadOnly(t *testing.T) {
	virtualization, rosetta := nativeCapabilities()
	if !virtualization.Supported && virtualization.Reason == "" {
		t.Fatal("missing framework capability has no diagnostic")
	}
	if !rosetta.Supported && rosetta.Reason == "" {
		t.Fatal("missing Rosetta capability has no installation guidance")
	}
	t.Logf("Virtualization.framework supported=%t; Rosetta Linux installed=%t", virtualization.Supported, rosetta.Supported)
}
