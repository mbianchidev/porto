// Package networktest provides loopback-only synthetic registry and proxy fixtures.
package networktest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/certificates"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Registry struct {
	URL       string
	Reference string
	Direct    string
	CA        []byte
	Proxy     string
	TLSProxy  string
	SOCKS     string
	ProxyHits atomic.Int64
	SOCKSHits atomic.Int64
}

func New(t testing.TB, platform ocispec.Platform) *Registry {
	t.Helper()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	manager := certificates.New(filepath.Join(root, "leaf.pem"), filepath.Join(root, "leaf.key"), ca, ca+".key")
	if _, err := manager.Ensure("registry.synthetic.test", "host.lima.internal"); err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(ocispec.Image{Platform: platform, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: nil}})
	if err != nil {
		t.Fatal(err)
	}
	digest := func(data []byte) string {
		sum := sha256.Sum256(data)
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	configDigest := digest(configuration)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, configDigest, len(configuration)))
	manifestDigest := digest(manifest)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []byte
		switch r.URL.Path {
		case "/v2/":
			data = []byte("{}")
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		case "/v2/synthetic/manifests/v1", "/v2/synthetic/manifests/" + manifestDigest:
			data = manifest
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
		case "/v2/synthetic/blobs/" + configDigest:
			data = configuration
			w.Header().Set("Content-Type", "application/vnd.oci.image.config.v1+json")
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	}))
	server.TLS = manager.TLSConfig()
	pair, err := server.TLS.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS.Certificates = append(server.TLS.Certificates, *pair)
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	caBytes, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &Registry{URL: "https://registry.synthetic.test:" + port, Reference: "registry.synthetic.test:" + port + "/synthetic:v1",
		Direct: server.Listener.Addr().String() + "/synthetic:v1", CA: caBytes}
	var connections sync.Map
	track := func(connection net.Conn) {
		connections.Store(connection, true)
	}
	t.Cleanup(func() {
		connections.Range(func(key, _ any) bool { _ = key.(net.Conn).Close(); return true })
	})
	tunnel := func(client net.Conn) {
		defer client.Close()
		defer connections.Delete(client)
		upstream, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			return
		}
		defer upstream.Close()
		track(upstream)
		defer connections.Delete(upstream)
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, client); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = upstream.Close()
		_ = client.Close()
		<-done
	}
	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "registry.synthetic.test:"+port {
			http.Error(w, "synthetic proxy refuses any other destination", http.StatusForbidden)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		track(client)
		fixture.ProxyHits.Add(1)
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffer.Flush(); err != nil {
			_ = client.Close()
			return
		}
		tunnel(client)
	})
	proxy := httptest.NewServer(proxyHandler)
	t.Cleanup(proxy.Close)
	fixture.Proxy = proxy.URL
	tlsProxy := httptest.NewUnstartedServer(proxyHandler)
	tlsProxy.TLS = server.TLS.Clone()
	tlsProxy.StartTLS()
	t.Cleanup(tlsProxy.Close)
	fixture.TLSProxy = tlsProxy.URL
	socks, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socks.Close() })
	fixture.SOCKS = "socks5h://" + socks.Addr().String()
	go func() {
		for {
			connection, err := socks.Accept()
			if err != nil {
				return
			}
			track(connection)
			go func() {
				valid := false
				defer func() {
					if !valid {
						_ = connection.Close()
						connections.Delete(connection)
					}
				}()
				_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
				var header [2]byte
				if _, err := io.ReadFull(connection, header[:]); err != nil || header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(connection, methods); err != nil {
					return
				}
				if _, err := connection.Write([]byte{5, 0}); err != nil {
					return
				}
				var request [4]byte
				if _, err := io.ReadFull(connection, request[:]); err != nil || request != [4]byte{5, 1, 0, 3} {
					return
				}
				var length [1]byte
				if _, err := io.ReadFull(connection, length[:]); err != nil {
					return
				}
				host := make([]byte, int(length[0])+2)
				if _, err := io.ReadFull(connection, host); err != nil ||
					string(host[:len(host)-2]) != "registry.synthetic.test" ||
					strconv.Itoa(int(host[len(host)-2])<<8|int(host[len(host)-1])) != port {
					return
				}
				if _, err := connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
					return
				}
				fixture.SOCKSHits.Add(1)
				_ = connection.SetDeadline(time.Time{})
				valid = true
				tunnel(connection)
			}()
		}
	}()
	return fixture
}
