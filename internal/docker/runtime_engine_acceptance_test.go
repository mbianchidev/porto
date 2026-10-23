package docker

import (
	"errors"
	"os"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
)

func (fixture *runtimeDataFixture) engineLifecycle(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA") != "1" {
		t.Skip("engine restart is restricted to the independently owned disposable CI VM; existing engines are never stopped")
	}
	if err := fixture.vms.Stop(fixture.ctx, fixture.instance.Name); err != nil {
		t.Fatalf("stop the completed, independently owned source fixture before destination restart: %v", err)
	}
	name := fixture.prefix + "-engine-lifecycle"
	fixture.extraSelections = append(fixture.extraSelections, dataops.Selection{Kind: "volume", Name: name, Destination: name})
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
	stopped, err := fixture.manager.PrepareEngineUpdate(fixture.ctx)
	if err != nil || !stopped {
		t.Fatalf("owned update preparation failed: stopped=%v, %v", stopped, err)
	}
	if len(fixture.native.List()) != 0 {
		t.Fatal("engine update left active native attachments")
	}
	if _, err := os.Lstat(record.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update left a stale host folder: %v", err)
	}
	if _, _, err := fixture.manager.NativeFilesResource(fixture.ctx, "volume", name, true); err == nil {
		t.Fatal("a stopped engine advertised accessible native data")
	}
	_, running, err := fixture.manager.limaInstanceStatus(fixture.ctx)
	if err != nil || running {
		t.Fatalf("capability probing restarted the stopped engine: running=%v, %v", running, err)
	}
	if err := fixture.manager.StartEngine(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	target, err = fixture.nativeTarget("volume", name, true)
	if err != nil {
		t.Fatal(err)
	}
	reconnected, err := fixture.native.Attach(fixture.ctx, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.Identity != record.Identity {
		t.Fatal("engine restart retargeted a recreated native identity")
	}
	if err := fixture.native.Close(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if len(fixture.native.List()) != 0 {
		t.Fatal("daemon shutdown left native attachments")
	}
}
