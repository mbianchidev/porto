package daemon

import (
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func TestResourceSaverDoesNotTreatRuntimeSystemMountsAsUserState(t *testing.T) {
	system := []portodocker.ContainerMount{
		{Type: "proc", Destination: "/proc"},
		{Type: "tmpfs", Destination: "/dev"},
		{Type: "sysfs", Destination: "/sys"},
		{Type: "bind", Destination: "/etc/hosts"},
		{Type: "bind", Destination: "/etc/hostname"},
		{Type: "bind", Destination: "/etc/resolv.conf"},
	}
	if saverHasStatefulMounts(system) {
		t.Fatal("standard OCI system mounts made every stateless service ineligible")
	}
	for _, mount := range []portodocker.ContainerMount{
		{Type: "volume", Destination: "/data"},
		{Type: "bind", Destination: "/workspace"},
	} {
		if !saverHasStatefulMounts(append(system, mount)) {
			t.Fatal("mounted application data did not require separate opt-in")
		}
	}
}
