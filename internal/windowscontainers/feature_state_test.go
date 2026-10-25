package windowscontainers

import "testing"

func TestNativeFeatureStateRequiresUnambiguousCompletedState(t *testing.T) {
	for _, test := range []struct {
		output  string
		enabled bool
		valid   bool
	}{
		{"Feature Name : Containers\r\nState : Enabled\r\n", true, true},
		{"Feature Name : Containers\nState : Disabled\n", false, true},
		{"State : Disabled with Payload Removed\n", false, true},
		{"State : Enable Pending\n", false, false},
		{"State : Disable Pending\n", false, false},
		{"State : Enabled\nState : Disabled\n", false, false},
		{"Feature Name : Containers\n", false, false},
		{"State : unknown\n", false, false},
	} {
		enabled, err := windowsFeatureEnabled([]byte(test.output))
		if enabled != test.enabled || (err == nil) != test.valid {
			t.Errorf("feature %q = %t, %v; want enabled=%t, valid=%t", test.output, enabled, err, test.enabled, test.valid)
		}
	}
}
