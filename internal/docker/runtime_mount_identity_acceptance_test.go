package docker

import (
	"runtime"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
)

func (fixture *runtimeDataFixture) localMountIdentity(t *testing.T) {
	backend, err := fixture.manager.backend(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" || backend.limaInstance != "" {
		t.Skip("this cell uses a remote confined bridge, not a local kernel bind")
	}
	name := fixture.prefix + "-kernel-mount"
	fixture.extraSelections = append(fixture.extraSelections, dataops.Selection{Kind: "volume", Destination: name})
	if _, err := fixture.manager.CreateVolume(fixture.ctx, name, "local", map[string]string{"porto.test.suite": fixture.prefix}); err != nil {
		t.Fatal(err)
	}
	target, err := fixture.nativeTarget("volume", name, true)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fixture.native.Attach(fixture.ctx, target, true)
	if err != nil {
		t.Fatal(err)
	}
	fixture.backendPathCommand(t, "sudo", "-n", "mount", "-t", "tmpfs", "-o", "size=1m,mode=0700", "tmpfs", record.Path)
	if _, err := fixture.native.Get(fixture.ctx, record.ID); err == nil {
		t.Fatal("a foreign replacement bind was reported connected")
	}
	if err := fixture.native.Detach(fixture.ctx, record.ID); err == nil {
		t.Fatal("cleanup detached an unrelated replacement filesystem")
	}
	fixture.backendPathCommand(t, "sudo", "-n", "umount", "--", record.Path)
	if err := fixture.native.Detach(fixture.ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	target, err = fixture.nativeTarget("volume", name, true)
	if err != nil {
		t.Fatal(err)
	}
	record, err = fixture.native.Attach(fixture.ctx, target, true)
	if err != nil {
		t.Fatal(err)
	}
	fixture.backendPathCommand(t, "sudo", "-n", "umount", "--", record.Path)
	disconnected, err := fixture.native.Get(fixture.ctx, record.ID)
	if err == nil || disconnected.State != "disconnected" {
		t.Fatalf("an empty unmounted directory was returned as a native path: %+v, %v", disconnected, err)
	}
	if err := fixture.native.Detach(fixture.ctx, record.ID); err != nil {
		t.Fatal(err)
	}
}
