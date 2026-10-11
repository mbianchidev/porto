package nativefiles

import (
	"context"
	"errors"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type fixtureRunner struct{}

func (fixtureRunner) Run(context.Context, runtimes.Command) ([]byte, error) {
	return nil, errors.New("synthetic unavailable driver")
}

func TestNativeAttachmentRejectsImagesWritesAndStaleIdentities(t *testing.T) {
	manager := New(t.TempDir(), fixtureRunner{})
	image := datafiles.Resource{Kind: "image", Name: "fixture-image", ID: "fixture-digest", ReadOnly: true}
	target := Target{Resource: image, Identity: image.Fingerprint()}
	if _, err := manager.Attach(context.Background(), target, true); !errors.Is(err, datafiles.ErrUnsupported) {
		t.Fatalf("image writes were accepted: %v", err)
	}
	target.Identity = "reused-name"
	if _, err := manager.Attach(context.Background(), target, false); !errors.Is(err, datafiles.ErrConflict) {
		t.Fatalf("stale identity was accepted: %v", err)
	}
}

func TestNativeLocationsAreCollisionSafeForUnicodeAndRecreatedResources(t *testing.T) {
	first := datafiles.Resource{Kind: "volume", Name: "fixture-\u03b4", ID: "source-a"}
	second := first
	second.ID = "source-b"
	if attachmentID(first, false) == attachmentID(second, false) || attachmentID(first, true) == attachmentID(first, false) {
		t.Fatal("resource identity or write boundary did not change its native location")
	}
}

func TestNativeUnavailableCapabilityProvidesAnActionableAlternative(t *testing.T) {
	manager := New(t.TempDir(), fixtureRunner{})
	manager.lookPath = func(string) (string, error) { return "", errors.New("synthetic missing SSHFS") }
	capability := manager.Capability(context.Background(), false)
	if capability.Supported || capability.Message == "" || capability.Fallback == "" {
		t.Fatalf("unsafe success-shaped capability: %+v", capability)
	}
}
