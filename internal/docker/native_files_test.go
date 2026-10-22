package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestNativeVolumeVerificationReadsEachPinnedIdentityWithoutRediscovery(t *testing.T) {
	resource := datafiles.Resource{
		Kind: "volume", Name: "synthetic-volume", ID: "synthetic-generation",
		CreatedAt: "2026-01-01T00:00:00Z", Backend: "lima/default",
	}
	descriptor := runtimefiles.Descriptor{
		Resource: resource, VolumeIdentity: resource.ID,
		RootPath: "/synthetic/volumes/synthetic-volume/_data", Namespace: "default",
		Address: "/synthetic/containerd.sock", Owner: "synthetic-files-owner",
	}
	response, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	var helperError error
	var identities, discoveries atomic.Int32
	runner := &fakeRunner{
		handler: func(command runtimes.Command) ([]byte, error) {
			switch strings.Join(command.Args, " ") {
			case "list porto-engine --json":
				return []byte(`{"name":"porto-engine","status":"Running"}`), nil
			case `shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`:
				return []byte("synthetic-owner\n"), nil
			default:
				t.Errorf("verification performed unexpected discovery: %+v", command)
				return nil, errors.New("unexpected synthetic command")
			}
		},
		streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
			identities.Add(1)
			var envelope runtimefiles.Envelope
			if err := json.NewDecoder(command.StdinReader).Decode(&envelope); err != nil {
				return nil, err
			}
			if envelope.Descriptor.RootPath != descriptor.RootPath || envelope.Descriptor.VolumeIdentity != descriptor.VolumeIdentity ||
				envelope.Descriptor.Owner != descriptor.Owner || envelope.Descriptor.Address != descriptor.Address ||
				envelope.Descriptor.Namespace != descriptor.Namespace || envelope.Descriptor.Resource != resource ||
				envelope.Request.Action != "volume-identity" || envelope.Request.Identity != resource.Fingerprint() ||
				!slices.Contains(command.Args, limaOwnedRuntimeCommand) {
				t.Error("verification discarded the pinned resource or guest ownership guard")
			}
			if helperError != nil {
				return nil, helperError
			}
			return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: response})
		},
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.fileDescriptorReader = func(context.Context, string, string) (runtimefiles.Descriptor, error) {
		discoveries.Add(1)
		return descriptor, nil
	}
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
	target, err := manager.NativeFilesTarget(context.Background(), "volume", resource.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	var pending sync.WaitGroup
	for range 5 {
		pending.Go(func() {
			if err := target.Verify(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	pending.Wait()
	if identities.Load() != 5 || discoveries.Load() != 1 {
		t.Fatalf("identity checks were cached/coalesced or repeated discovery: helpers=%d discoveries=%d", identities.Load(), discoveries.Load())
	}
	for _, test := range []struct {
		name   string
		change func(*datafiles.Resource)
	}{
		{"recreated metadata", func(current *datafiles.Resource) { current.ID = "replacement-generation" }},
		{"new birth time", func(current *datafiles.Resource) { current.CreatedAt = "2026-01-02T00:00:00Z" }},
		{"different volume", func(current *datafiles.Resource) { current.Name = "foreign-volume" }},
		{"different backend", func(current *datafiles.Resource) { current.Backend = "foreign/default" }},
		{"different kind", func(current *datafiles.Resource) { current.Kind = "container" }},
		{"missing identity", func(current *datafiles.Resource) { current.ID = "" }},
		{"missing birth time", func(current *datafiles.Resource) { current.CreatedAt = "" }},
		{"read-only replacement", func(current *datafiles.Resource) { current.ReadOnly = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := resource
			test.change(&current)
			response, err = json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			before := identities.Load()
			if err := target.Verify(context.Background()); !errors.Is(err, datafiles.ErrConflict) || identities.Load() != before+1 {
				t.Fatalf("verification reused stale identity or accepted a replacement: %v", err)
			}
		})
	}
	response = []byte("invalid synthetic JSON")
	if err := target.Verify(context.Background()); err == nil {
		t.Fatal("malformed helper identity was accepted")
	}
	helperError = errors.New("synthetic missing volume metadata")
	if err := target.Verify(context.Background()); !errors.Is(err, helperError) {
		t.Fatalf("helper failure became successful cached verification: %v", err)
	}
	if discoveries.Load() != 1 {
		t.Fatal("pinned identity verification repeated full volume discovery")
	}
	for _, kind := range []string{"volume", "image", "container"} {
		t.Run("unbound-"+kind, func(t *testing.T) {
			descriptor.Resource = resource
			descriptor.Resource.Kind = kind
			descriptor.Resource.ReadOnly = kind == "image"
			descriptor.VolumeIdentity = ""
			beforeDiscovery, beforeIdentity := discoveries.Load(), identities.Load()
			unbound, err := manager.NativeFilesTarget(context.Background(), kind, resource.Name, false)
			if err != nil {
				t.Fatal(err)
			}
			descriptor.Resource.ID = "replacement-generation"
			if err := unbound.Verify(context.Background()); !errors.Is(err, datafiles.ErrConflict) ||
				discoveries.Load() != beforeDiscovery+2 || identities.Load() != beforeIdentity {
				t.Fatalf("an unbound resource bypassed full descriptor verification: %v", err)
			}
		})
	}
}

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
