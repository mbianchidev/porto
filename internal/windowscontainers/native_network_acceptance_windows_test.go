//go:build windows

package windowscontainers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/networking/networktest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func testNativeWindowsNetworking(t *testing.T, parent context.Context, installed Installation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	previous, err := LoadNetworking(installed)
	if err != nil || previous == nil {
		t.Fatalf("native owner networking was not established before acceptance: %v", err)
	}
	fixture := networktest.New(t, ocispec.Platform{OS: "windows", Architecture: "amd64",
		OSVersion: fmt.Sprintf("10.0.%d.0", installed.Host.Build)})
	client := ClientFor(installed)
	t.Cleanup(client.CloseIdleConnections)
	request := func(ctx context.Context, method, path string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, nil)
		if err != nil {
			return 0, "", err
		}
		response, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		return response.StatusCode, string(data), err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if applied, err := ConfigureNetworking(cleanup, *previous); err != nil || !applied {
			t.Errorf("restore original native owner networking: %v", err)
		}
	}()
	for _, proxy := range []string{fixture.Proxy, fixture.TLSProxy, fixture.SOCKS} {
		configuration := networking.RuntimeConfiguration{Config: networking.Config{Mode: "manual", HTTPSProxy: proxy}, CAPEM: fixture.CA}
		if applied, err := ConfigureNetworking(ctx, configuration); err != nil || !applied {
			t.Fatalf("native policy apply: %v", err)
		}
		path := "/images/create?" + url.Values{"fromImage": {fixture.Reference}}.Encode()
		if code, body, err := request(ctx, http.MethodPost, path); err != nil || code != http.StatusOK {
			t.Fatalf("actual native registry pull through %s: HTTP %d %s %v", proxy, code, body, err)
		}
		if code, body, err := request(ctx, http.MethodDelete, "/images/"+url.PathEscape(fixture.Reference)+"?force=true"); err != nil || code != http.StatusOK {
			t.Fatalf("remove only native proxy fixture image: HTTP %d %s %v", code, body, err)
		}
		configuration.CAPEM = nil
		if applied, err := ConfigureNetworking(ctx, configuration); err != nil || !applied {
			t.Fatalf("native trust removal: %v", err)
		}
		code, body, err := request(ctx, http.MethodPost, path)
		if err != nil || code == http.StatusOK || (!strings.Contains(body, "x509") && !strings.Contains(body, "certificate")) {
			t.Fatalf("native CA removal did not produce a verified TLS failure: HTTP %d %s %v", code, body, err)
		}
	}
	if fixture.ProxyHits.Load() == 0 || fixture.SOCKSHits.Load() == 0 {
		t.Fatal("native worker did not traverse actual HTTP and SOCKS registry proxies")
	}
	t.Log("actual native HTTP/HTTPS/SOCKS proxy and reversible registry CA acceptance passed")
}
