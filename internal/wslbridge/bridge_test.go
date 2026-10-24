//go:build !windows

package wslbridge

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateBridgeReplacesSpoofedDistributionHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(DistributionHeader) != "synthetic selected" || r.Header.Get(OwnerHeader) != "synthetic-owner" {
			t.Errorf("distribution identity was not pinned: %v", r.Header)
		}
		_, _ = io.Copy(w, r.Body)
	}))
	defer backend.Close()
	directory, err := os.MkdirTemp("", "pw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configuration := Config{Distribution: "synthetic selected", Owner: "synthetic-owner", Socket: filepath.Join(directory, "docker.sock")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, configuration, func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(backend.URL, "http://"))
		})
	}()
	client := socketClient(configuration.Socket)
	defer client.CloseIdleConnections()
	var response *http.Response
	err = nil
	for range 100 {
		request, _ := http.NewRequest(http.MethodPost, "http://porto/containers/create", strings.NewReader("synthetic payload"))
		request.Header.Set(DistributionHeader, "non-opted-in")
		request.Header.Set(OwnerHeader, "foreign-owner")
		response, err = client.Do(request)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "synthetic payload" {
		t.Fatalf("bridge response = %q, %v", data, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("WSL bridge did not close on cancellation")
	}
}
