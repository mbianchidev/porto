package docker

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	controlapi "github.com/moby/buildkit/api/services/control"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type buildHistoryServer struct {
	controlapi.UnimplementedControlServer
	events []*controlapi.BuildHistoryEvent
	delay  time.Duration
}

func (s *buildHistoryServer) ListenBuildHistory(
	_ *controlapi.BuildHistoryRequest,
	stream grpc.ServerStreamingServer[controlapi.BuildHistoryEvent],
) error {
	if s.delay > 0 {
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-stream.Context().Done():
			return context.Cause(stream.Context())
		}
	}
	for _, event := range s.events {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	return nil
}

func TestBuildsReturnsBuildKitHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	controlapi.RegisterControlServer(server, &buildHistoryServer{events: []*controlapi.BuildHistoryEvent{
		{
			Type: controlapi.BuildHistoryEventType_COMPLETE,
			Record: &controlapi.BuildHistoryRecord{
				Ref:           "failed-build",
				FrontendAttrs: map[string]string{"filename": "Dockerfile", "platform": "linux/arm64"},
				CreatedAt:     timestamppb.New(now.Add(-2 * time.Minute)),
				CompletedAt:   timestamppb.New(now.Add(-time.Minute)),
				Error:         &statuspb.Status{Message: "build failed"},
			},
		},
		{
			Type: controlapi.BuildHistoryEventType_COMPLETE,
			Record: &controlapi.BuildHistoryRecord{
				Ref:           "successful-build",
				FrontendAttrs: map[string]string{"filename": "services/api/Dockerfile", "platform": "linux/amd64"},
				Exporters: []*controlapi.Exporter{{
					Type:  "image",
					Attrs: map[string]string{"name": "porto/api:latest"},
				}},
				CreatedAt:   timestamppb.New(now.Add(-30 * time.Second)),
				CompletedAt: timestamppb.New(now),
			},
		},
	}})
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.dialBuildKit = func(context.Context) (net.Conn, error) {
		return listener.Dial()
	}
	builds, err := manager.Builds(context.Background())
	if err != nil {
		t.Fatalf("list builds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("builds = %+v", builds)
	}
	if builds[0].ID != "successful-build" || builds[0].Name != "porto/api:latest" || builds[0].Status != "succeeded" {
		t.Fatalf("unexpected successful build: %+v", builds[0])
	}
	if builds[0].Duration != "30s" || builds[0].Platform != "linux/amd64" {
		t.Fatalf("unexpected successful build metadata: %+v", builds[0])
	}
	if builds[1].ID != "failed-build" || builds[1].Status != "failed" {
		t.Fatalf("unexpected failed build: %+v", builds[1])
	}
}

func TestBuildsKeepsLimaTunnelAfterGRPCDial(t *testing.T) {
	manager, assertClosed := buildHistoryTunnelFixture(t, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	builds, err := manager.Builds(ctx)
	if err != nil {
		t.Fatalf("read BuildKit history through the Lima tunnel: %v", err)
	}
	if len(builds) != 1 || builds[0].ID != "test-build" || builds[0].Status != "succeeded" {
		t.Fatalf("unexpected tunneled build history: %+v", builds)
	}
	assertClosed()
}

func TestBuildsCancellationClosesLimaTunnel(t *testing.T) {
	manager, assertClosed := buildHistoryTunnelFixture(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := manager.Builds(ctx)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("history cancellation = %v, want deadline exceeded", err)
	}
	assertClosed()
}

func buildHistoryTunnelFixture(t *testing.T, delay time.Duration) (*Manager, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	controlapi.RegisterControlServer(server, &buildHistoryServer{
		delay: delay,
		events: []*controlapi.BuildHistoryEvent{{
			Type: controlapi.BuildHistoryEventType_COMPLETE,
			Record: &controlapi.BuildHistoryRecord{
				Ref:         "test-build",
				CreatedAt:   timestamppb.New(time.Unix(0, 0)),
				CompletedAt: timestamppb.New(time.Unix(10, 0)),
			},
		}},
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	var mu sync.Mutex
	var tunnels []*commandConn
	manager := New(&fakeRunner{})
	manager.dialBuildKit = func(ctx context.Context) (net.Conn, error) {
		command := limaBuildKitStdioCommand(ctx, "test-engine")
		command.Path = os.Args[0]
		command.Args = []string{os.Args[0], "-test.run=^TestCommandConnHelperProcess$"}
		command.Err = nil
		command.Env = process.WithEnvironment(os.Environ(),
			"PORTO_TEST_COMMAND_CONN=proxy",
			"PORTO_TEST_TUNNEL_ADDRESS="+listener.Addr().String(),
		)
		connection, err := dialCommandConn(ctx, "BuildKit tunnel", command, buildKitAddr("host"), buildKitAddr("guest"))
		if err == nil {
			mu.Lock()
			tunnels = append(tunnels, connection.(*commandConn))
			mu.Unlock()
		}
		return connection, err
	}
	return manager, func() {
		t.Helper()
		mu.Lock()
		connections := append([]*commandConn(nil), tunnels...)
		mu.Unlock()
		if len(connections) == 0 {
			t.Fatal("history request did not open a tunnel")
		}
		for _, connection := range connections {
			select {
			case <-connection.done:
			case <-time.After(3 * time.Second):
				t.Fatal("history request leaked its tunnel subprocess")
			}
		}
	}
}
