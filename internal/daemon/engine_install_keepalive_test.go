package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type engineIdleProgressConnection struct {
	net.Conn
}

func (connection engineIdleProgressConnection) Read(data []byte) (int, error) {
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return 0, err
	}
	return connection.Conn.Read(data)
}

func TestEngineInstallProgressKeepsQuietProvisioningAliveWithoutInventingStages(t *testing.T) {
	server, runner, _ := engineProgressFixture(t, false, "limactl")
	release := make(chan struct{})
	defer close(release)
	runner.release = release
	finished := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.streamEngineInstallProgress(w, r, 20*time.Millisecond)
		close(finished)
	}))
	defer httpServer.Close()
	dialer := &net.Dialer{}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return engineIdleProgressConnection{connection}, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodPost, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var current engineInstallEvent
	for {
		if err := decoder.Decode(&current); err != nil {
			t.Fatal(err)
		}
		if current.Progress != nil && current.Progress.Stage == "downloading" {
			break
		}
	}
	for range 3 {
		var heartbeat engineInstallEvent
		if err := decoder.Decode(&heartbeat); err != nil {
			t.Fatalf("quiet provisioning lost its bounded idle transport before the installation budget: %v", err)
		}
		if heartbeat.Type != "progress" || heartbeat.Progress == nil || *heartbeat.Progress != *current.Progress ||
			heartbeat.Status != nil || heartbeat.Error != nil {
			t.Fatalf("keepalive invented progress or completion: %+v", heartbeat)
		}
	}
	select {
	case <-finished:
		t.Fatal("keepalive completed an installation that was still blocked")
	default:
	}
	release <- struct{}{}
	terminal := false
	for {
		var event engineInstallEvent
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
		if terminal || event.Type == "complete" {
			t.Fatalf("keepalive outlived or masked the failed installation: %+v", event)
		}
		terminal = event.Type == "error"
	}
	if !terminal {
		t.Fatal("installation failure disappeared after keepalive")
	}
}
