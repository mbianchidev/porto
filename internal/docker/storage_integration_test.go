package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type storageIntegrationRunner struct{ helper string }

func (r storageIntegrationRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	return (runtimes.ExecRunner{}).Run(ctx, command)
}

func (r storageIntegrationRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	for index, argument := range command.Args {
		if argument == `exec sudo -n -- "$HOME/.local/bin/porto-runtime-helper" files` {
			command.Args = append([]string(nil), command.Args...)
			command.Args[index] = `exec sudo -n -- "` + r.helper + `" files`
			break
		}
	}
	return (runtimes.ExecRunner{}).RunStreaming(ctx, command, emit)
}

func TestStorageVolumeLifecycleIntegration(t *testing.T) {
	if os.Getenv("PORTO_STORAGE_INTEGRATION") != "1" {
		t.Skip("set PORTO_STORAGE_INTEGRATION=1 and PORTO_STORAGE_TEST_HELPER to test synthetic volumes in an already running owned Lima engine")
	}
	if runtime.GOOS == "windows" {
		t.Skip("this live fixture uses Lima's host CLI on macOS/Linux")
	}
	localHelper := os.Getenv("PORTO_STORAGE_TEST_HELPER")
	if !filepath.IsAbs(localHelper) {
		t.Fatal("PORTO_STORAGE_TEST_HELPER must point to this revision's cross-built Linux helper")
	}
	base := New(nil)
	state, err := base.readEngineState()
	if err != nil || state.Mode != "lima" || state.Instance != engineInstanceName {
		t.Fatalf("an existing Porto-owned Lima engine is required; none was started: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	exists, running, err := base.limaInstanceStatus(ctx)
	if err != nil || !exists || !running {
		t.Fatalf("an already running engine is required: exists=%v running=%v error=%v", exists, running, err)
	}
	if err := base.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		t.Fatal(err)
	}
	execRunner := runtimes.ExecRunner{}
	output, err := execRunner.Run(ctx, runtimes.Command{
		Name: "limactl", Args: []string{"shell", "--workdir=/", state.Instance, "--", "mktemp", "-d", "/tmp/porto-storage-fixture-XXXXXXXX"},
	})
	if err != nil {
		t.Fatal(err)
	}
	guestDirectory := strings.TrimSpace(string(output))
	if !strings.HasPrefix(guestDirectory, "/tmp/porto-storage-fixture-") || strings.ContainsAny(guestDirectory, "\r\n\"\\") {
		t.Fatal("the synthetic helper directory could not be proven")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, err := execRunner.Run(cleanup, runtimes.Command{Name: "limactl", Args: []string{
			"shell", "--workdir=/", state.Instance, "--", "rm", "-r", "--", guestDirectory,
		}}); err != nil {
			t.Errorf("remove exact synthetic helper directory: %v", err)
		}
	})
	guestHelper := guestDirectory + "/helper"
	if _, err := execRunner.Run(ctx, runtimes.Command{Name: "limactl", Args: []string{"copy", localHelper, state.Instance + ":" + guestHelper}}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRunner.Run(ctx, runtimes.Command{Name: "limactl", Args: []string{"shell", state.Instance, "--", "chmod", "700", guestHelper}}); err != nil {
		t.Fatal(err)
	}
	if testBinary := os.Getenv("PORTO_STORAGE_RUNTIME_TEST"); testBinary != "" {
		if !filepath.IsAbs(testBinary) {
			t.Fatal("PORTO_STORAGE_RUNTIME_TEST must be an absolute cross-built Linux test executable")
		}
		guestTests := guestDirectory + "/runtime-tests"
		for _, args := range [][]string{
			{"copy", testBinary, state.Instance + ":" + guestTests},
			{"shell", state.Instance, "--", "chmod", "700", guestTests},
			{"shell", "--workdir=/", state.Instance, "--", guestTests, "-test.run", "^(TestRuntimeMetadataUsage|TestNativeVolumeIdentity)", "-test.v"},
		} {
			if output, err := execRunner.Run(ctx, runtimes.Command{Name: "limactl", Args: args}); err != nil {
				t.Fatalf("verify Linux metadata/identity fixtures: %v: %s", err, output)
			}
		}
	}
	manager := NewWithStateDir(storageIntegrationRunner{helper: guestHelper}, base.stateDir)
	home := t.TempDir()
	t.Setenv("PORTO_HOME", home)
	transfers := filepath.Join(home, "transfers")
	if err := os.Mkdir(transfers, 0o700); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "synthetic.txt"), []byte("synthetic volume round trip\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(source, "nested", "synthetic.txt"), filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/synthetic.txt", filepath.Join(source, "symlink")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(transfers, "source.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, exportErr := datafiles.Export(ctx, file, source, datafiles.Resource{Kind: "volume", Name: "synthetic-source", ID: "synthetic-local"})
	if err := errors.Join(exportErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	archive, err := InspectVolumeArchive(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "porto-storage-fixture-" + strings.TrimPrefix(guestDirectory, "/tmp/porto-storage-fixture-")
	var created []datafiles.Resource
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, resource := range created {
			current, err := manager.FileDescriptor(cleanup, "volume", resource.Name)
			if err != nil || current.Resource.Fingerprint() != resource.Fingerprint() {
				t.Errorf("refusing cleanup of changed synthetic volume %s: %v", resource.Name, err)
				continue
			}
			if err := manager.RemoveVolume(cleanup, resource.Name, false); err != nil {
				t.Errorf("remove exact synthetic volume %s: %v", resource.Name, err)
			}
		}
	})
	progress := func(string, int64) error { return nil }
	for _, suffix := range []string{"source", "restored"} {
		destination := prefix + "-" + suffix
		if _, err := manager.ImportVolume(ctx, destination, archive.Path, archive.SHA256, progress); err != nil {
			t.Fatalf("import into synthetic volume %s: %v", destination, err)
		}
		descriptor, err := manager.FileDescriptor(ctx, "volume", destination)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, descriptor.Resource)
		exported, err := manager.ExportVolume(ctx, descriptor.Resource, descriptor.Resource.Fingerprint(), filepath.Join(transfers, "captured-"+suffix+".tar"), progress)
		if err != nil {
			t.Fatal(err)
		}
		root, file, err := datafiles.OpenManagedArchive(exported.Path)
		if err != nil {
			t.Fatal(err)
		}
		restored, validateErr := datafiles.Validate(ctx, file)
		if err := errors.Join(validateErr, file.Close(), root.Close()); err != nil {
			t.Fatal(err)
		}
		portableEntries := func(entries []datafiles.Entry) []datafiles.Entry {
			result := append([]datafiles.Entry(nil), entries...)
			for index := range result {
				if result[index].Type == "symlink" {
					// Linux symlink mode bits are fixed; every transferable field must match.
					result[index].Mode = 0
				}
			}
			return result
		}
		if restored.Bytes != manifest.Bytes || !reflect.DeepEqual(portableEntries(restored.Entries), portableEntries(manifest.Entries)) {
			t.Fatalf("live export/import changed contents, permissions, ownership or supported links: %+v, want %+v", restored, manifest)
		}
		measured, err := manager.VolumeStorage(ctx, destination)
		if err != nil || measured.LogicalBytes == nil || *measured.LogicalBytes != manifest.Bytes {
			t.Fatalf("live volume allocation did not reconcile: %+v, %v", measured, err)
		}
		archive = exported
	}
	cancelled := prefix + "-cancelled"
	_, err = manager.ImportVolume(ctx, cancelled, archive.Path, archive.SHA256, func(string, int64) error { return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled synthetic import error=%v", err)
	}
	volumes, err := manager.Volumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range volumes {
		if volume.Name == cancelled {
			t.Fatal("cancelled import left an owned temporary volume behind")
		}
	}
}
