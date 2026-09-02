package resources

import "testing"

func TestParseCPUAndMemoryQuantities(t *testing.T) {
	cpu, err := ParseCPU("250m")
	if err != nil || cpu != 250 {
		t.Fatalf("ParseCPU(250m) = %d, %v; want 250, nil", cpu, err)
	}
	memory, err := ParseBytes("1.5GiB")
	if err != nil || memory != 1610612736 {
		t.Fatalf("ParseBytes(1.5GiB) = %d, %v; want 1610612736, nil", memory, err)
	}
}
