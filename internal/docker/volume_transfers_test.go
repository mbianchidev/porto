package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestTemporaryVolumeCleanupUsesOneFreshInspectionWithinDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		temporary := dataops.VolumeTemporary{Owner: strings.Repeat("a", 32), Destination: "synthetic-staging"}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		inspections, removals, clears := 0, 0, 0
		manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
			switch strings.Join(command.Args, " ") {
			case "volume inspect synthetic-staging":
				inspections++
				select {
				case <-time.After(12 * time.Second):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return json.Marshal([]map[string]any{{
					"Name": temporary.Destination, "Mountpoint": "/synthetic/staging/_data",
					"Labels": map[string]string{transferOwnerLabel: temporary.Owner},
				}})
			case "volume rm synthetic-staging":
				removals++
				select {
				case <-time.After(8 * time.Second):
					return nil, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			default:
				return nil, nil
			}
		}})
		manager.SetVolumeTransferLedger(nil, func(ctx context.Context, current dataops.VolumeTemporary) error {
			if current != temporary {
				t.Errorf("cleared another reservation: %+v", current)
			}
			clears++
			return ctx.Err()
		}, func(context.Context) ([]dataops.VolumeTemporary, error) {
			return []dataops.VolumeTemporary{temporary}, nil
		})
		started := time.Now()
		if err := manager.RecoverVolumeTransfers(ctx); err != nil {
			t.Fatalf("owned cleanup exceeded its unchanged thirty-second budget: %v", err)
		}
		if elapsed := time.Since(started); elapsed != 20*time.Second {
			t.Fatalf("cleanup took %s instead of one inspection and removal", elapsed)
		}
		if inspections != 1 || removals != 1 || clears != 1 {
			t.Fatalf("cleanup inspection/removal/clear counts = %d/%d/%d, want 1/1/1", inspections, removals, clears)
		}
	})
}

func TestTemporaryVolumeCleanupRetainsReservationWhenRemovalFails(t *testing.T) {
	temporary := dataops.VolumeTemporary{Owner: strings.Repeat("b", 32), Destination: "synthetic-staging"}
	failure := errors.New("synthetic volume removal failure")
	inspections, cleared := 0, false
	manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "volume inspect synthetic-staging":
			inspections++
			return json.Marshal([]map[string]any{{
				"Name": temporary.Destination, "Labels": map[string]string{transferOwnerLabel: temporary.Owner},
			}})
		case "volume rm synthetic-staging":
			return nil, failure
		default:
			return nil, nil
		}
	}})
	manager.SetVolumeTransferLedger(nil, func(context.Context, dataops.VolumeTemporary) error {
		cleared = true
		return nil
	}, func(context.Context) ([]dataops.VolumeTemporary, error) {
		return []dataops.VolumeTemporary{temporary}, nil
	})
	err := manager.RecoverVolumeTransfers(context.Background())
	if !errors.Is(err, failure) || cleared || inspections != 1 {
		t.Fatalf("failed cleanup lost its error or recoverable reservation: err=%v cleared=%v inspections=%d", err, cleared, inspections)
	}
}

func TestTemporaryVolumeCleanupKeepsRemovalGuardsWithInspectedMetadata(t *testing.T) {
	for _, boundary := range []string{"native mount", "stopped container reference", "changed name"} {
		t.Run(boundary, func(t *testing.T) {
			temporary := dataops.VolumeTemporary{Owner: strings.Repeat("c", 32), Destination: "synthetic-staging"}
			root := "/synthetic/staging/_data"
			removed, cleared := false, false
			inspections := 0
			manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				switch strings.Join(command.Args, " ") {
				case "volume inspect synthetic-staging":
					inspections++
					name := temporary.Destination
					if boundary == "changed name" {
						name = "another-volume"
					}
					return json.Marshal([]map[string]any{{
						"Name": name, "Mountpoint": root,
						"Labels": map[string]string{transferOwnerLabel: temporary.Owner},
					}})
				case "volume rm synthetic-staging":
					removed = true
				}
				return nil, nil
			}})
			if boundary == "native mount" {
				manager.SetNativeFilesGuard(func(string, string) error { return ErrConflict })
			}
			var containers []Container
			if boundary == "stopped container reference" {
				containers = []Container{{
					ID: "synthetic-container", Name: "synthetic-owner", State: "exited",
					MountDetails: []ContainerMount{{Source: root, Destination: "/data"}},
				}}
			}
			runtime := newFakeContainerRuntime(containers)
			runtime.Subscribe(context.Background())
			manager.inventoryCancel = func() {}
			manager.inventory = &containerInventory{
				snapshot: ContainerSnapshot{Available: true}, runtimeClient: runtime,
			}
			manager.SetVolumeTransferLedger(nil, func(context.Context, dataops.VolumeTemporary) error {
				cleared = true
				return nil
			}, func(context.Context) ([]dataops.VolumeTemporary, error) {
				return []dataops.VolumeTemporary{temporary}, nil
			})
			err := manager.RecoverVolumeTransfers(context.Background())
			if !errors.Is(err, ErrConflict) || removed || cleared || inspections != 1 {
				t.Fatalf("cleanup bypassed %s: err=%v removed=%v cleared=%v inspections=%d", boundary, err, removed, cleared, inspections)
			}
			if runtime.closed {
				t.Fatal("owned cleanup closed the borrowed inventory connection")
			}
		})
	}
}

func TestCancelledVolumeCreationCleansOnlyProvenTemporaryResources(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("PORTO_HOME", state)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "fixture.txt"), []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}

			archiveDirectory := filepath.Join(state, "transfers")
			if err := os.Mkdir(archiveDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(archiveDirectory, "fixture.tar")
			file, err := os.Create(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			_, exportErr := datafiles.Export(context.Background(), file, source, datafiles.Resource{Kind: "volume", Name: "fixture", ID: "fixture-source"})
			if err := errors.Join(exportErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			archive, err := InspectVolumeArchive(context.Background(), archivePath)
			if err != nil {
				t.Fatal(err)
			}
			owner := ""
			removed := false
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				args := strings.Join(command.Args, " ")
				switch {
				case strings.HasPrefix(args, "volume create "):
					for _, value := range command.Args {
						if strings.HasPrefix(value, transferOwnerLabel+"=") {
							owner = strings.TrimPrefix(value, transferOwnerLabel+"=")
						}
					}
					return nil, context.Canceled
				case args == "volume inspect restored-fixture":
					if !owned {
						owner = "foreign-owner"
					}
					return json.Marshal([]map[string]any{{"Name": "restored-fixture", "Labels": map[string]string{transferOwnerLabel: owner}}})
				case args == "volume rm restored-fixture":
					removed = true
					return nil, nil
				default:
					return nil, nil
				}
			}}
			_, err = New(runner).ImportVolume(context.Background(), "restored-fixture", archivePath, archive.SHA256, func(string, int64) error { return nil })
			if !errors.Is(err, context.Canceled) || removed != owned {
				t.Fatalf("cancelled import error=%v removed=%v expected=%v", err, removed, owned)
			}
		})
	}
}

func TestVolumePreviewUsesFreshOwnersAndProtectsManagedData(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  string
		labels map[string]string
	}{
		{"new running writer", "running", nil},
		{"stopped managed cluster", "exited", map[string]string{"io.x-k8s.kind.cluster": "synthetic-cluster"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
			root := "/synthetic/volumes/synthetic-volume/_data"
			container := Container{
				ID: "container-1", Name: "synthetic-writer", State: test.state,
				Labels: test.labels, ComposeProject: "synthetic-stack", ComposeService: "database",
				MountDetails: []ContainerMount{{Source: root, Destination: "/data", Options: []string{"rw"}}},
			}
			runtime := newFakeContainerRuntime([]Container{container})
			runtime.Subscribe(context.Background())
			manager := New(&fakeRunner{})
			manager.fileDescriptorReader = func(context.Context, string, string) (runtimefiles.Descriptor, error) {
				return runtimefiles.Descriptor{Resource: resource, RootPath: root}, nil
			}
			manager.inventoryCancel = func() {}
			manager.inventory = &containerInventory{
				snapshot:      ContainerSnapshot{Available: true, Containers: []Container{}},
				runtimeClient: runtime,
			}
			preview, err := manager.PreviewVolume(context.Background(), dataops.Request{
				Action: "volume-export", Resource: resource,
			})
			if err != nil || len(preview.Owners) != 1 || preview.Owners[0].ComposeProject != "synthetic-stack" {
				t.Fatalf("fresh container/Compose ownership was omitted: %+v, %v", preview, err)
			}
			_, err = manager.PreviewVolume(context.Background(), dataops.Request{Action: "volume-empty", Resource: resource})
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("protected data could be emptied: %v", err)
			}
			if runtime.closed {
				t.Fatal("a per-request ownership check closed the inventory connection")
			}
		})
	}
}

func TestVolumeRemovalProtectsFreshBindPathOwnersEvenWithForce(t *testing.T) {
	root := "/synthetic/volumes/synthetic-volume/_data"
	runtime := newFakeContainerRuntime([]Container{{
		ID: "container-1", Name: "synthetic-owner", State: "exited",
		MountDetails: []ContainerMount{{Source: root, Destination: "/data"}},
	}})
	runtime.Subscribe(context.Background())
	removed := false
	manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if strings.HasPrefix(strings.Join(command.Args, " "), "volume inspect ") {
			return json.Marshal([]map[string]any{{"Name": "synthetic-volume", "Mountpoint": root}})
		}
		if strings.HasPrefix(strings.Join(command.Args, " "), "volume rm ") {
			removed = true
		}
		return nil, nil
	}})
	manager.inventoryCancel = func() {}
	manager.inventory = &containerInventory{
		snapshot:      ContainerSnapshot{Available: true, Containers: []Container{}},
		runtimeClient: runtime,
	}
	err := manager.RemoveVolume(context.Background(), "synthetic-volume", true)
	if !errors.Is(err, ErrConflict) || removed {
		t.Fatalf("force bypassed a fresh stopped-container mount reference: %v, removed=%v", err, removed)
	}
	if runtime.closed {
		t.Fatal("volume removal closed the borrowed inventory connection")
	}
}

func TestVolumeRemovalPreviewBindsTheExactVolumeIdentity(t *testing.T) {
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	manager := New(&fakeRunner{})
	manager.fileDescriptorReader = func(context.Context, string, string) (runtimefiles.Descriptor, error) {
		return runtimefiles.Descriptor{Resource: resource, RootPath: "/synthetic/volume/_data"}, nil
	}
	preview, err := manager.PreviewVolume(context.Background(), dataops.Request{Action: "volume-remove", Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Identity != resource.Fingerprint() || !strings.Contains(preview.Consequences, resource.Name) {
		t.Fatalf("removal consequences or exact source are missing: %+v", preview)
	}
	resource.ID = "recreated-volume"
	_, err = manager.PreviewVolume(context.Background(), preview.Request)
	if !errors.Is(err, datafiles.ErrConflict) {
		t.Fatalf("reused volume name did not invalidate removal preview: %v", err)
	}
}

func TestInterruptedVolumeTransfersRecoverOnlyPersistedOwnedDestinations(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			temporary := dataops.VolumeTemporary{Owner: strings.Repeat("a", 32), Destination: "synthetic-staging"}
			removed, cleared := false, false
			manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				switch strings.Join(command.Args, " ") {
				case "volume inspect synthetic-staging":
					owner := temporary.Owner
					if !owned {
						owner = "foreign"
					}
					return json.Marshal([]map[string]any{{"Name": temporary.Destination, "Labels": map[string]string{transferOwnerLabel: owner}}})
				case "volume rm synthetic-staging":
					removed = true
				}
				return nil, nil
			}})
			manager.SetVolumeTransferLedger(nil, func(context.Context, dataops.VolumeTemporary) error {
				cleared = true
				return nil
			}, func(context.Context) ([]dataops.VolumeTemporary, error) {
				return []dataops.VolumeTemporary{temporary}, nil
			})
			err := manager.RecoverVolumeTransfers(context.Background())
			if (err == nil) != owned || removed != owned || cleared != owned {
				t.Fatalf("recovery did not preserve proven ownership: err=%v removed=%v cleared=%v", err, removed, cleared)
			}
		})
	}
}
