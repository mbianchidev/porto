package docker

import (
	"encoding/json"
	"testing"
)

func TestLifecycleSettledRecognizesCompletedActions(t *testing.T) {
	running := Container{State: "running", PID: 10, ExitAt: "2026-01-01T00:00:00Z"}
	exited := Container{State: "exited", ExitAt: "2026-01-01T00:00:00Z"}
	for _, test := range []struct {
		action        string
		before, after Container
		want          bool
	}{
		{"stop", running, running, false},
		{"stop", running, exited, true},
		{"start", exited, exited, false},
		{"start", exited, Container{State: "running", PID: 11}, true},
		{"start", exited, Container{State: "exited", ExitAt: "2026-01-02T00:00:00Z"}, true},
		{"restart", running, running, false},
		{"restart", running, Container{State: "running", PID: 12}, true},
		{"pause", running, Container{State: "paused", PID: 10}, true},
		{"unpause", Container{State: "paused", PID: 10}, Container{State: "paused", PID: 10}, false},
	} {
		if got := lifecycleSettled(test.action)(test.before, test.after); got != test.want {
			t.Errorf("%s %s -> %s = %t, want %t", test.action, test.before.State, test.after.State, got, test.want)
		}
	}
	if lifecycleSettled("remove") != nil {
		t.Fatal("removal has its own inventory confirmation")
	}
}

func TestDockerInspectPrefixesContainerNamesOnly(t *testing.T) {
	for _, test := range []struct {
		input string
		image bool
		want  string
	}{
		{`{"Name":"web","Config":{}}`, false, "/web"},
		{`{"Name":"/web","Config":{}}`, false, "/web"},
		{`{"Name":"","Config":{}}`, false, ""},
		{`{"Name":"web"}`, true, "web"},
	} {
		output, err := dockerInspectForVersion(json.RawMessage(test.input), "1.47", test.image)
		if err != nil {
			t.Fatal(err)
		}
		var document struct{ Name string }
		if err := json.Unmarshal(output, &document); err != nil {
			t.Fatal(err)
		}
		if document.Name != test.want {
			t.Fatalf("inspect %s (image %t) Name = %q, want %q", test.input, test.image, document.Name, test.want)
		}
	}
}
