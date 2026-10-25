//go:build windows

package windowscontainers

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/networking"
	"golang.org/x/sys/windows"
)

func TestNativeWindowsContainersNetworkCredentialsUseProtectedDPAPI(t *testing.T) {
	if os.Getenv("PORTO_TEST_NATIVE_WINDOWS_CONTAINERS") != "1" {
		t.Skip("requires the disposable native Windows acceptance row")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("native DPAPI ownership acceptance requires the elevated disposable row")
	}
	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	i := Installation{OwnerSID: sid, Root: filepath.Join(t.TempDir(), "owned")}
	if err := createProtectedRoot(i); err != nil {
		t.Fatal(err)
	}
	configuration := networking.RuntimeConfiguration{Config: networking.Config{
		Mode: "manual", HTTPSProxy: "http://synthetic:mock-password@proxy.example.test:3128",
	}}
	if err := SaveNetworking(i, configuration); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(i.Root, "networking.dpapi.json")
	if err := protectedTree(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte("mock-password")) || bytes.Contains(data, []byte("proxy.example.test")) {
		t.Fatalf("native proxy credentials were not encrypted: %v", err)
	}
	restored, err := LoadNetworking(i)
	if err != nil || restored == nil || restored.Config.HTTPSProxy != configuration.Config.HTTPSProxy {
		t.Fatalf("native protected networking restore: %v", err)
	}
	i.OwnerSID = "S-1-5-21-111-222-333-1001"
	if _, err := LoadNetworking(i); err == nil {
		t.Fatal("DPAPI networking was adopted under a different runtime owner")
	}
}
