package networktest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func trustedFixtureTLS(t *testing.T, fixture *Registry) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(fixture.CA) {
		t.Fatal("fixture CA is not a valid certificate")
	}
	return &tls.Config{RootCAs: pool, ServerName: "registry.synthetic.test"}
}

func TestRegistryServesVerifiableOCIImageAndHeadResponses(t *testing.T) {
	platform := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	fixture := New(t, platform)
	destination, _, _ := strings.Cut(fixture.Direct, "/")
	transport := &http.Transport{
		TLSClientConfig: trustedFixtureTLS(t, fixture),
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, destination)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	read := func(method, path string) ([]byte, *http.Response) {
		t.Helper()
		request, err := http.NewRequest(method, fixture.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("registry response = %d, %v, %v", response.StatusCode, err, closeErr)
		}
		return content, response
	}
	content, response := read(http.MethodGet, "/v2/synthetic/manifests/v1")
	if response.Header.Get("Docker-Content-Digest") != digest.FromBytes(content).String() {
		t.Fatal("fixture manifest digest does not identify its served bytes")
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	head, response := read(http.MethodHead, "/v2/synthetic/manifests/v1")
	if len(head) != 0 || response.ContentLength != int64(len(content)) {
		t.Fatalf("HEAD response = %d body bytes, length %d", len(head), response.ContentLength)
	}
	configuration, _ := read(http.MethodGet, "/v2/synthetic/blobs/"+manifest.Config.Digest.String())
	if manifest.Config.Digest != digest.FromBytes(configuration) || manifest.Config.Size != int64(len(configuration)) {
		t.Fatal("fixture image configuration does not match its OCI descriptor")
	}
	var image ocispec.Image
	if err := json.Unmarshal(configuration, &image); err != nil {
		t.Fatal(err)
	}
	if image.OS != platform.OS || image.Architecture != platform.Architecture {
		t.Fatalf("fixture changed the requested platform: %s/%s", image.OS, image.Architecture)
	}
}

func TestRegistryTraversesHTTPHTTPSAndSOCKSTunnels(t *testing.T) {
	fixture := New(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	for _, test := range []struct {
		name  string
		proxy string
		socks bool
	}{
		{name: "HTTP", proxy: fixture.Proxy},
		{name: "HTTPS", proxy: fixture.TLSProxy},
		{name: "SOCKS", proxy: fixture.SOCKS, socks: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, err := url.Parse(test.proxy)
			if err != nil {
				t.Fatal(err)
			}
			before := fixture.ProxyHits.Load()
			if test.socks {
				before = fixture.SOCKSHits.Load()
			}
			transport := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: trustedFixtureTLS(t, fixture)}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
			response, err := client.Get(fixture.URL + "/v2/")
			if err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if err != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(content) != "{}" {
				t.Fatalf("tunneled registry result = %d, %q, %v, %v", response.StatusCode, content, err, closeErr)
			}
			after := fixture.ProxyHits.Load()
			if test.socks {
				after = fixture.SOCKSHits.Load()
			}
			if after != before+1 {
				t.Fatalf("request used %d new tunnels, want one", after-before)
			}
		})
	}
}

func TestSOCKSProxyRejectsForeignDestinations(t *testing.T) {
	fixture := New(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	proxy, err := url.Parse(fixture.SOCKS)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: trustedFixtureTLS(t, fixture)}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err := client.Get("https://foreign.synthetic.test:443/v2/")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || fixture.SOCKSHits.Load() != 0 {
		t.Fatalf("foreign SOCKS request returned %v and opened %d tunnels", err, fixture.SOCKSHits.Load())
	}
}

func TestProxyRejectsNonConnectAndForeignDestinations(t *testing.T) {
	fixture := New(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	proxy, err := url.Parse(fixture.Proxy)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := url.Parse(fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, proxy.Host)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for _, test := range []struct {
		method string
		host   string
	}{
		{method: http.MethodGet, host: registry.Host},
		{method: http.MethodConnect, host: "foreign.synthetic.test:443"},
	} {
		request, err := http.NewRequest(test.method, "http://"+test.host, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusForbidden || fixture.ProxyHits.Load() != 0 {
			t.Fatalf("forbidden proxy request returned %d and opened %d tunnels", response.StatusCode, fixture.ProxyHits.Load())
		}
	}
}
