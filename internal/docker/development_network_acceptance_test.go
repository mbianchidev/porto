package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/networking/networktest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func testDevelopmentNetworking(t *testing.T, parent context.Context, manager *Manager) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Log("Proxy/registry mutation acceptance is restricted to the disposable CI engine")
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	backend, err := manager.backend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "networking.json")
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	network := networking.New(root)
	fixture := networktest.New(t, ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH})
	certificate, err := network.ImportCertificate(fixture.CA)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(proxy string, trusted bool) error {
		configuration := networking.Config{Mode: "manual", HTTPSProxy: proxy}
		if trusted {
			configuration.RegistryCAs = []string{certificate.Reference}
		}
		if err := network.Save(configuration); err != nil {
			return err
		}
		if backend.limaInstance != "" {
			if strings.HasPrefix(proxy, "https://127.0.0.1:") {
				err := manager.ApplyNetworking(ctx)
				if err == nil || !strings.Contains(err.Error(), "TLS-encrypted loopback proxy") {
					return errors.New("guest encrypted-loopback proxy must report its unverified guest hostname without changing builder trust")
				}
				// The owner-side image resolver can use this TLS proxy.
				// The separate builder needs an explicitly guest-reachable name.
				return nil
			}
			return manager.ApplyNetworking(ctx)
		}
		return nil
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, reference := range []string{fixture.Reference, fixture.Direct} {
			if err := manager.RemoveImage(cleanup, reference, true); err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("remove synthetic network image: %v", err)
			}
		}
		if err := network.Save(networking.Config{Mode: "off"}); err != nil {
			t.Errorf("clear synthetic networking: %v", err)
			return
		}
		if backend.limaInstance != "" {
			if err := manager.ApplyNetworking(cleanup); err != nil {
				t.Errorf("clear only owned guest networking: %v", err)
				return
			}
		}
		if readErr == nil {
			if err := os.WriteFile(path, previous, 0o600); err != nil {
				t.Errorf("restore prior disposable networking settings: %v", err)
			} else if backend.limaInstance != "" {
				if err := manager.ApplyNetworking(cleanup); err != nil {
					t.Errorf("restore prior owned guest networking: %v", err)
				}
			}
		} else if err := os.Remove(path); err != nil {
			t.Errorf("remove only synthetic networking settings: %v", err)
		}
	}()
	for _, proxy := range []string{fixture.Proxy, fixture.TLSProxy, fixture.SOCKS} {
		if err := apply(proxy, true); err != nil {
			t.Fatal(err)
		}
		if err := manager.PullImage(ctx, fixture.Reference, "linux/"+runtime.GOARCH); err != nil {
			t.Fatalf("%s registry pull through %s: %v", backend.description, strings.SplitN(proxy, ":", 2)[0], err)
		}
		if err := manager.RemoveImage(ctx, fixture.Reference, true); err != nil {
			t.Fatal(err)
		}
		if err := apply(proxy, false); err != nil {
			t.Fatal(err)
		}
		if err := manager.PullImage(ctx, fixture.Reference, "linux/"+runtime.GOARCH); err == nil {
			t.Fatal("removing registry CA did not reject the actual backend pull")
		} else if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("backend CA removal failed for an unrelated reason: %v", err)
		}
	}
	if fixture.ProxyHits.Load() == 0 || fixture.SOCKSHits.Load() == 0 {
		t.Fatal("backend did not traverse both real HTTP and SOCKS proxies")
	}
	if backend.limaInstance == "" {
		if err := apply(fixture.Proxy, true); err != nil {
			t.Fatal(err)
		}
		before := fixture.ProxyHits.Load()
		if err := manager.PullImage(ctx, fixture.Direct, "linux/"+runtime.GOARCH); err != nil {
			t.Fatalf("local loopback registry bypass: %v", err)
		}
		if fixture.ProxyHits.Load() != before {
			t.Fatal("local registry was accidentally sent through proxy")
		}
	}
	t.Logf("real %s HTTP/HTTPS/SOCKS proxy, CA removal and local bypass acceptance passed on %s/%s",
		backend.description, runtime.GOOS, runtime.GOARCH)
}
