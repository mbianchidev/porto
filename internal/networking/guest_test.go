package networking

import (
	"strings"
	"testing"
)

func TestGuestProxyUsesHostGatewayWithoutChangingHostProxy(t *testing.T) {
	manager := New(t.TempDir())
	manager.secrets = memorySecrets{}
	if err := manager.Save(Config{Mode: "manual", HTTPProxy: "http://synthetic:mock-password@127.0.0.1:3128"}); err != nil {
		t.Fatal(err)
	}
	guest, err := manager.GuestConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(guest.Environment, "\n"), "http://synthetic:mock-password@host.lima.internal:3128") {
		t.Fatalf("guest proxy environment did not use the host gateway: %v", guest.Environment)
	}
	host, err := manager.Environment()
	if err != nil || !strings.Contains(strings.Join(host, "\n"), "@127.0.0.1:3128") {
		t.Fatalf("host proxy was changed: %v, %v", host, err)
	}
}
