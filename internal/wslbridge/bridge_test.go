//go:build !windows

package wslbridge

import (
	"context"
	"errors"
	"fmt"
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

func TestControlAcknowledgementIsFlushedBeforeStop(t *testing.T) {
	response := httptest.NewRecorder()
	stopped := false
	err := acknowledgeControl(response, "synthetic-owner", func() {
		stopped = true
		if !response.Flushed {
			t.Fatal("bridge stopped before flushing its control acknowledgement")
		}
	})
	if err != nil || !stopped {
		t.Fatalf("control acknowledgement = %v, stopped=%t", err, stopped)
	}
	result := response.Result()
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK || result.Header.Get(OwnerHeader) != "synthetic-owner" || result.ContentLength != 0 {
		t.Fatalf("invalid complete control acknowledgement: %d, %q, length=%d",
			result.StatusCode, result.Header.Get(OwnerHeader), result.ContentLength)
	}
}

type failedControlFlush struct {
	*httptest.ResponseRecorder
}

func (*failedControlFlush) FlushError() error { return io.ErrClosedPipe }

func TestControlAcknowledgementReportsFlushFailureAndStillStops(t *testing.T) {
	response := &failedControlFlush{httptest.NewRecorder()}
	stopped := false
	err := acknowledgeControl(response, "synthetic-owner", func() { stopped = true })
	if !errors.Is(err, io.ErrClosedPipe) || !stopped {
		t.Fatalf("failed acknowledgement = %v, stopped=%t", err, stopped)
	}
}

func TestPrivateBridgeStopAcknowledgesAndReleasesOwnership(t *testing.T) {
	directory, err := os.MkdirTemp("", "pw-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	for round := range 25 {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			configuration := Config{
				Distribution: "synthetic-selected",
				Owner:        fmt.Sprintf("synthetic-owner-%d", round),
				Socket:       filepath.Join(directory, fmt.Sprintf("s%d.sock", round)),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, configuration, func(context.Context) (net.Conn, error) {
					return nil, errors.New("synthetic transport must not be used by a control request")
				})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(time.Second):
					t.Error("owned bridge did not finish shutdown")
				}
			})
			ready := false
			for range 100 {
				if control(ctx, configuration, "status") == nil {
					ready = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("owned bridge did not become ready")
			}
			foreign := configuration
			foreign.Owner = "foreign-owner"
			if err := Stop(ctx, foreign); err == nil {
				t.Fatal("unrelated owner stopped the bridge")
			}
			if err := control(ctx, configuration, "status"); err != nil {
				t.Fatalf("foreign stop changed the owned bridge: %v", err)
			}
			if err := Stop(ctx, configuration); err != nil {
				t.Fatalf("owned stop request lost its acknowledgement: %v", err)
			}
			if _, err := os.Lstat(configuration.Socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stop left its socket behind: %v", err)
			}
			lease, err := lock(ctx, configuration.Socket+".serve.lock", false)
			if err != nil {
				t.Fatalf("stop returned before releasing ownership: %v", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

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
