package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestNativeFileRequestChecksGuestOwnershipWithoutConsumingTransferInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the ownership guard executes in the Linux guest")
	}
	for _, owner := range []string{"synthetic-owner", "foreign-owner"} {
		t.Run(owner, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string]string{
				".porto-engine-owner":             owner + "\n",
				".local/bin/sudo":                 "#!/bin/sh\n[ \"$1\" = '-n' ] && [ \"$2\" = '--' ] || exit 2\nshift 2\nexec \"$@\"\n",
				".local/bin/porto-runtime-helper": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/invoked\"\ncat\n",
			} {
				if err := os.WriteFile(filepath.Join(home, name), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			runner := &fakeRunner{streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
				if command.Name != "limactl" || len(command.Args) < 5 {
					t.Fatalf("unexpected native transport command: %+v", command)
				}
				return (runtimes.ExecRunner{}).RunStreaming(ctx, runtimes.Command{
					Name: command.Args[4], Args: command.Args[5:], StdinReader: command.StdinReader,
					Env: []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")},
				}, emit)
			}}
			manager := NewWithStateDir(runner, t.TempDir())
			manager.lookPath = func(string) (string, error) { return "", errors.New("no fallback CLI in this fixture") }
			if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
				t.Fatal(err)
			}
			manager.inventoryCancel = func() {}
			manager.inventory = &containerInventory{
				snapshot: ContainerSnapshot{Available: true},
				runtimeClient: &grpcContainerRuntime{commandSource: commandBackend{
					name: "limactl", limaInstance: engineInstanceName, ownerID: "synthetic-owner",
				}},
			}
			descriptor := runtimefiles.Descriptor{Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "synthetic-identity"}}
			request := datafiles.Request{Action: "read", Identity: descriptor.Resource.Fingerprint(), Path: "synthetic.txt"}
			header, err := json.Marshal(runtimefiles.Envelope{Descriptor: descriptor, Request: request})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("synthetic\x00binary\xffpayload")
			expected := append(append(header, '\n'), payload...)
			var output bytes.Buffer
			err = manager.RunFileRequest(ctx, descriptor, request, bytes.NewReader(payload), &output)
			if len(runner.commands) != 1 {
				t.Fatalf("the file request did not use exactly one ownership-checked helper: %v, %+v", err, runner.commands)
			}
			invoked, readErr := os.ReadFile(filepath.Join(home, "invoked"))
			if owner == "synthetic-owner" {
				if err != nil || readErr != nil || string(invoked) != "files\n" || !bytes.Equal(output.Bytes(), expected) {
					t.Fatalf("the native helper changed its arguments or transfer stream: run=%v read=%v args=%q output=%q", err, readErr, invoked, output.Bytes())
				}
			} else if err == nil || !strings.Contains(err.Error(), "ownership changed") || !errors.Is(readErr, os.ErrNotExist) || output.Len() != 0 {
				t.Fatalf("an unverified engine executed the native helper: run=%v read=%v output=%q", err, readErr, output.Bytes())
			}
		})
	}
}

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
