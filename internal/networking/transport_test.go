package networking

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/networking/networktest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNetworkTransportHTTPHTTPSAndSOCKSChangesKeepTLSVerification(t *testing.T) {
	registry := networktest.New(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	for _, address := range []string{registry.Proxy, registry.TLSProxy, registry.SOCKS} {
		t.Run(strings.SplitN(address, ":", 2)[0], func(t *testing.T) {
			configuration := RuntimeConfiguration{Config: Config{Mode: "manual", HTTPSProxy: address}, CAPEM: registry.CA}
			manager, err := NewRuntime(t.TempDir(), configuration)
			if err != nil {
				t.Fatal(err)
			}
			transport, err := manager.Transport()
			if err != nil {
				t.Fatal(err)
			}
			transport.DisableKeepAlives = true
			defer transport.CloseIdleConnections()
			response, err := (&http.Client{Transport: transport}).Get(registry.URL + "/v2/")
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || string(data) != "{}" {
				t.Fatalf("proxy transport failed: %d %s %v", response.StatusCode, data, err)
			}
			configuration.CAPEM = nil
			untrusted, err := NewRuntime(t.TempDir(), configuration)
			if err != nil {
				t.Fatal(err)
			}
			untrustedTransport, err := untrusted.Transport()
			if err != nil {
				t.Fatal(err)
			}
			defer untrustedTransport.CloseIdleConnections()
			if response, err := (&http.Client{Transport: untrustedTransport}).Get(registry.URL + "/v2/"); err == nil {
				response.Body.Close()
				t.Fatal("removing registry CA did not revoke transport trust")
			}
			configuration.CAPEM = registry.CA
			configuration.Config.NoProxy = ".synthetic.test"
			bypassed, err := NewRuntime(t.TempDir(), configuration)
			if err != nil {
				t.Fatal(err)
			}
			direct, err := bypassed.Transport()
			if err != nil {
				t.Fatal(err)
			}
			defer direct.CloseIdleConnections()
			host := strings.SplitN(registry.Direct, "/", 2)[0]
			direct.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", host)
			}
			before := registry.ProxyHits.Load() + registry.SOCKSHits.Load()
			response, err = (&http.Client{Transport: direct}).Get(registry.URL + "/v2/")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if registry.ProxyHits.Load()+registry.SOCKSHits.Load() != before {
				t.Fatal("bypass rules still traversed the proxy")
			}
		})
	}
	if registry.ProxyHits.Load() == 0 || registry.SOCKSHits.Load() == 0 {
		t.Fatal("transport matrix did not reach both HTTP and SOCKS proxy fixtures")
	}
}
