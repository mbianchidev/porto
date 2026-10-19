package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestNativeResourceCapabilityRejectsWritesBeforeAllocatingAnAttachment(t *testing.T) {
	for _, kind := range []string{"image", "container"} {
		t.Run(kind, func(t *testing.T) {
			commands := 0
			manager := New(&fakeRunner{handler: func(runtimes.Command) ([]byte, error) {
				commands++
				return nil, nil
			}})
			manager.fileDescriptorReader = func(context.Context, string, string) (runtimefiles.Descriptor, error) {
				return runtimefiles.Descriptor{Resource: datafiles.Resource{
					Kind: kind, Name: "synthetic", ID: "resource-1", ReadOnly: true,
				}}, nil
			}
			_, _, err := manager.NativeFilesResource(context.Background(), kind, "synthetic", true)
			if !errors.Is(err, datafiles.ErrUnsupported) || commands != 0 {
				t.Fatalf("capability probing mutated or accepted a read-only resource: %v, commands=%d", err, commands)
			}
		})
	}
}
