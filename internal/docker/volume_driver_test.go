package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestLocalVolumeDriverDoesNotInvokeUnsupportedNerdctlFlag(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if strings.Contains(strings.Join(command.Args, " "), "--driver") {
			t.Fatal("nerdctl's local volume store does not accept --driver")
		}
		return []byte("synthetic-volume\n"), nil
	}}
	manager := New(runner)
	volume, err := manager.CreateVolume(context.Background(), "synthetic-volume", "local", nil)
	if err != nil || volume.Driver != "local" || volume.Name != "synthetic-volume" {
		t.Fatalf("local volume = %+v, error=%v", volume, err)
	}
	if _, err := manager.CreateVolume(context.Background(), "synthetic-volume", "external", nil); !errors.Is(err, ErrUnsupported) || len(runner.commands) != 1 {
		t.Fatal("an unsupported volume driver reached resource creation")
	}
}
