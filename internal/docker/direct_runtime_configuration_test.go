package docker

import (
	"context"
	"errors"
	"testing"

	corecontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/mbianchidev/porto/internal/runtimes"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestDirectRuntimeUsesVerifiedBackendCgroupPlacement(t *testing.T) {
	for _, test := range []struct {
		name, info, path string
	}{
		{"rootless systemd", `{"OSType":"linux","Architecture":"x86_64","CgroupDriver":"systemd","SecurityOptions":["name=rootless"]}`, "user.slice:porto:synthetic"},
		{"rootful systemd", `{"OSType":"linux","Architecture":"aarch64","CgroupDriver":"systemd","SecurityOptions":[]}`, "system.slice:porto:synthetic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				if command.Name != "limactl" || command.Args[2] != "synthetic-owned" {
					t.Fatalf("runtime configuration was not read from the selected guest: %+v", command)
				}
				return []byte(test.info), nil
			}}
			runtime := &grpcContainerRuntime{lima: "synthetic-owned", runner: runner}
			info, options, specOptions, err := runtime.directRuntimeConfiguration(context.Background(), "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			if info.Architecture == "" || options == nil || !options.SystemdCgroup {
				t.Fatalf("backend runtime configuration = %+v, options=%+v", info, options)
			}
			spec := &oci.Spec{Linux: &specs.Linux{}}
			for _, option := range specOptions {
				if err := option(context.Background(), nil, &corecontainers.Container{ID: "synthetic"}, spec); err != nil {
					t.Fatal(err)
				}
			}
			if spec.Linux.CgroupsPath != test.path {
				t.Fatalf("cgroup path = %q, want %q", spec.Linux.CgroupsPath, test.path)
			}
		})
	}
}

func TestDirectRuntimeRefusesUnverifiedRootlessCgroupConfiguration(t *testing.T) {
	for _, info := range []string{
		`{"OSType":"linux","Architecture":"amd64","CgroupDriver":"cgroupfs","SecurityOptions":["name=rootless"]}`,
		`{"OSType":"linux","Architecture":"","CgroupDriver":"systemd"}`,
		`{"OSType":"linux","Architecture":"amd64","CgroupDriver":""}`,
	} {
		runtime := &grpcContainerRuntime{lima: "synthetic-owned", runner: &fakeRunner{
			handler: func(runtimes.Command) ([]byte, error) { return []byte(info), nil },
		}}
		if _, _, _, err := runtime.directRuntimeConfiguration(context.Background(), "synthetic"); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unverified backend configuration was accepted: %v", err)
		}
	}
}
