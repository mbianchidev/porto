package docker

import (
	"context"
	"net"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type buildHistoryServer struct {
	controlapi.UnimplementedControlServer
	events []*controlapi.BuildHistoryEvent
}

func (s *buildHistoryServer) ListenBuildHistory(
	_ *controlapi.BuildHistoryRequest,
	stream grpc.ServerStreamingServer[controlapi.BuildHistoryEvent],
) error {
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
