package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/wsl"
)

func TestWSLMountApplyRequiresExplicitEngineRestartConsent(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	manager := NewWithStateDir(runner, t.TempDir())
	if err := manager.ApplyWSLMounts(context.Background(), false); err == nil {
		t.Fatal("engine mount changes did not require explicit restart consent")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("engine was touched before restart consent: %+v", runner.commands)
	}
}

func TestWSLMountPlanPreservesUnrelatedHostMountOptions(t *testing.T) {
	configuration := []byte(`mounts:
- location: C:\Users\synthetic
  mountPoint: /host
  writable: false
  sshfs:
    cache: true
- location: old-owned-share
  mountPoint: /mnt/porto-wsl/old
  writable: true
`)
	expression, err := wslMountPlan(configuration,
		[]wsl.Share{{WindowsPath: "old-owned-share", GuestPath: "/mnt/porto-wsl/old"}},
		[]wsl.Share{{WindowsPath: `\\wsl.localhost\synthetic\home\dev`, GuestPath: "/mnt/porto-wsl/new"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"mountPoint":"/host"`, `"writable":false`, `"cache":true`, `"mountPoint":"/mnt/porto-wsl/new"`} {
		if !strings.Contains(expression, expected) {
			t.Fatalf("mount plan discarded %s: %s", expected, expression)
		}
	}
	if strings.Contains(expression, "old-owned-share") {
		t.Fatalf("stale owned share remains in plan: %s", expression)
	}
}
