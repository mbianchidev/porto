package resources

import "testing"

func TestParseHostCPUUsageUsesBackendTotalsWithoutDoubleCountingGuests(t *testing.T) {
	usage, err := ParseHostCPUUsage([]byte("cpu 10 20 30 40 50 60 70 80 900 1000\ncpu0 0 0 0 0\ncpu2 0 0 0 0\nintr 42\n"))
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalUsage != 3_600_000_000 || usage.OnlineCPUs != 2 {
		t.Fatalf("CPU counters = %+v", usage)
	}
}

func TestParseHostCPUUsageRejectsUnavailableAndCorruptCounters(t *testing.T) {
	for _, input := range []string{
		"", "cpu 1 2 3 4\n", "cpu0 1 2 3 4\n",
		"cpu 1 2 invalid 4\ncpu0 1 2 3 4\n",
		"cpu 18446744073709551615 1 0 0\ncpu0 1 2 3 4\n",
		"cpu 1844674407370955 0 0 0\ncpu0 1 2 3 4\n",
		"cpu 1 2 3 4\ncpu0 1 2 3 4\ncpu0 1 2 3 4\n",
	} {
		if _, err := ParseHostCPUUsage([]byte(input)); err == nil {
			t.Fatalf("accepted unavailable/corrupt CPU counters: %q", input)
		}
	}
}
