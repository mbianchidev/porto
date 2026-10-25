//go:build windows

package windowscontainers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/hcsshim/hcn"
	"golang.org/x/sys/windows"
)

func TestNativeWindowsNamedPipeOwnershipAndPermissionGates(t *testing.T) {
	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf(`\\.\pipe\porto-native-unit-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + sid + ")",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			defer connection.Close()
		}
	}()
	if err := requireAbsentPipe(path); !errors.Is(err, ErrOwnership) {
		t.Fatalf("foreign named pipe was considered free: %v", err)
	}
	anonymousPipeDenied(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := pipeProcess(connection, uint32(os.Getpid())+1); !errors.Is(err, ErrOwnership) {
		t.Fatalf("accepted wrong pipe server process: %v", err)
	}
	listener.Close()
	<-done
}

func TestNativeWindowsProtectedPathsRejectUserWritableACLs(t *testing.T) {
	path := t.TempDir()
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;AU)(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := protectedTree(path); !errors.Is(err, ErrOwnership) {
		t.Fatalf("user-writable runtime installation accepted: %v", err)
	}
}

func TestNativeWindowsHNSReceiptsRejectReplacedOrForeignNetworks(t *testing.T) {
	expected := Network{Name: "porto-owned-nat", ID: "11111111-1111-1111-1111-111111111111", Subnet: "10.253.1.0/24"}
	actual := hcn.HostComputeNetwork{Name: expected.Name, Id: expected.ID, Type: hcn.NAT,
		Ipams: []hcn.Ipam{{Type: "Static", Subnets: []hcn.Subnet{{IpAddressPrefix: expected.Subnet}}}}}
	if err := checkNetwork(expected, actual); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*hcn.HostComputeNetwork){
		func(n *hcn.HostComputeNetwork) { n.Id = "22222222-2222-2222-2222-222222222222" },
		func(n *hcn.HostComputeNetwork) { n.Name = "foreign-network" },
		func(n *hcn.HostComputeNetwork) { n.Type = hcn.Transparent },
		func(n *hcn.HostComputeNetwork) { n.Ipams = nil },
	} {
		changed := actual
		edit(&changed)
		if err := checkNetwork(expected, changed); !errors.Is(err, ErrOwnership) {
			t.Fatalf("foreign HNS identity accepted: %+v, %v", changed, err)
		}
	}
}

func TestNativeWindowsConfigurationUsesOnlyOwnedPaths(t *testing.T) {
	i := Installation{Root: t.TempDir(), OwnerSID: "S-1-5-21-111-222-333-1001", Generation: strings.Repeat("a", 32)}
	configuration := runtimeConfiguration(i)
	containerd, nerdctl := []byte(configuration["containerd.toml"]), []byte(configuration["nerdctl.toml"])
	for _, data := range [][]byte{containerd, nerdctl} {
		if strings.Contains(string(data), "containerd-containerd") || strings.Contains(string(data), "docker_engine") {
			t.Fatalf("native runtime points to a foreign/default pipe: %s", data)
		}
	}
	if strings.Contains(string(nerdctl), "\nruntime =") || !strings.Contains(string(nerdctl), "snapshotter = 'windows'") ||
		!slices.Contains(systemEnvironment(i), "NERDCTL_TOML="+filepath.Join(i.Deployment(), "nerdctl.toml")) ||
		!strings.Contains(serviceCommand(i), " windows-containers-worker "+i.OwnerSID) {
		t.Fatalf("native runtime configuration lost its identity: %s, %s", nerdctl, serviceCommand(i))
	}
}
