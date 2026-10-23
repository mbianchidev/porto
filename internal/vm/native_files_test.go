package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func TestNativeVMCapabilityChecksOwnershipStatusAndRecreatedIdentityWithoutBooting(t *testing.T) {
	runner := &recordingRunner{}
	manager := NewWithStateDir(runner, t.TempDir())
	metadata := Metadata{Name: "test-vm", Kind: "standalone", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := manager.writeMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	first, err := manager.NativeFilesResource(context.Background(), metadata.Name)
	if err != nil || first.Kind != "vm" || first.ID == "" {
		t.Fatalf("owned running VM capability unavailable: %+v, %v", first, err)
	}
	metadata.CreatedAt = metadata.CreatedAt.Add(time.Second)
	if err := manager.writeMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	second, err := manager.NativeFilesResource(context.Background(), metadata.Name)
	if err != nil || first.Fingerprint() == second.Fingerprint() {
		t.Fatalf("a recreated VM retained the previous native identity: %+v, %v", second, err)
	}
	runner.status = "Stopped"
	if _, err := manager.NativeFilesResource(context.Background(), metadata.Name); !errors.Is(err, datafiles.ErrUnsupported) {
		t.Fatalf("a stopped VM was advertised as accessible: %v", err)
	}
	runner.status = "Running"
	metadata.Kind = "kubernetes"
	if err := manager.writeMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.NativeFilesResource(context.Background(), metadata.Name); err == nil {
		t.Fatal("native access adopted a VM owned by another subsystem")
	}
	for _, command := range runner.commands {
		if strings.Join(command.Args, " ") != "list --json" {
			t.Fatalf("capability probing changed the VM or installed a helper: %+v", command)
		}
	}
}
