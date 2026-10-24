package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	const prefix, suffix = `.mounts = ("`, `" | @base64d | from_json)`
	if !strings.HasPrefix(expression, prefix) || !strings.HasSuffix(expression, suffix) {
		t.Fatalf("mount plan must decode JSON instead of unescaping paths in the expression: %s", expression)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(expression, prefix), suffix))
	if err != nil {
		t.Fatal(err)
	}
	var mounts []struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal(data, &mounts); err != nil || len(mounts) != 2 {
		t.Fatalf("mount plan = %s: %v", data, err)
	}
	if mounts[1].Location != `\\wsl.localhost\synthetic\home\dev` {
		t.Fatalf("UNC mount lost its absolute path prefix: %q", mounts[1].Location)
	}
	for _, expected := range []string{`"mountPoint":"/host"`, `"writable":false`, `"cache":true`, `"mountPoint":"/mnt/porto-wsl/new"`} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("mount plan discarded %s: %s", expected, data)
		}
	}
	if strings.Contains(string(data), "old-owned-share") {
		t.Fatalf("stale owned share remains in plan: %s", data)
	}
}
