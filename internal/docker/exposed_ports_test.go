package docker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestCompatibilityCreatePreservesExposedPortsWithoutUnsupportedFlags(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		arguments := strings.Join(command.Args, " ")
		if strings.Contains(arguments, "--expose") ||
			!strings.Contains(arguments, portoExposedPortsLabel+`=["5000/tcp"]`) {
			t.Fatalf("exposed metadata was not retained compatibly: %s", arguments)
		}
		return []byte("synthetic-container\n"), nil
	}}
	if _, err := New(runner).CreateContainer(context.Background(), CreateContainerRequest{
		Image: "synthetic:latest", Expose: []string{"5000/tcp"},
	}); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"Config":{"Labels":{"io.porto.container.exposed-ports":"[\"5000/tcp\"]"},"ExposedPorts":{"80/tcp":{}}}}`)
	inspected, err := dockerInspectForVersion(raw, "1.47", false)
	if err != nil || !strings.Contains(string(inspected), `"5000/tcp":{}`) || !strings.Contains(string(inspected), `"80/tcp":{}`) {
		t.Fatalf("exposed inspect metadata = %s, error=%v", inspected, err)
	}
}
