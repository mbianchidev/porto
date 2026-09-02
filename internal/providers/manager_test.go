package providers

import (
	"context"
	"strings"
	"testing"
)

func TestInstallRejectsUnknownProvider(t *testing.T) {
	_, err := New(nil).Install(context.Background(), "unknown")
	if err == nil || !strings.Contains(err.Error(), "unknown runtime provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestK0sProviderUsesLima(t *testing.T) {
	provider, ok := findTool("k0s")
	if !ok {
		t.Fatal("k0s provider is missing")
	}
	if provider.command != "limactl" || provider.formula != "lima" {
		t.Fatalf("unexpected k0s provider: %+v", provider)
	}
}

func TestK9sProviderUsesNativeBinary(t *testing.T) {
	provider, ok := findTool("k9s")
	if !ok {
		t.Fatal("k9s provider is missing")
	}
	if provider.command != "k9s" || provider.formula != "k9s" {
		t.Fatalf("unexpected k9s provider: %+v", provider)
	}
}

func TestQEMUProviderUsesArchitectureBinary(t *testing.T) {
	provider, ok := findTool("qemu")
	if !ok {
		t.Fatal("qemu provider is missing")
	}
	if !strings.HasPrefix(provider.command, "qemu-system-") || provider.formula != "qemu" {
		t.Fatalf("unexpected qemu provider: %+v", provider)
	}
}
