package docker

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	apitypes "github.com/moby/buildkit/api/types"
	"github.com/moby/buildkit/solver/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type buildKitPlatformServer struct {
	controlapi.UnimplementedControlServer
	mu           sync.Mutex
	ready        bool
	active       bool
	completed    bool
	noWorkers    bool
	workerErr    error
	historyErr   error
	historyCalls int
}

func (s *buildKitPlatformServer) ListWorkers(context.Context, *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workerErr != nil {
		return nil, s.workerErr
	}
	if s.noWorkers {
		return &controlapi.ListWorkersResponse{}, nil
	}
	platforms := []*pb.Platform{{OS: "linux", Architecture: "arm64"}}
	if s.ready {
		platforms = append(platforms,
			&pb.Platform{OS: "linux", Architecture: "arm"},
			&pb.Platform{OS: "linux", Architecture: "arm", Variant: "v6"},
			&pb.Platform{OS: "linux", Architecture: "386"},
		)
	}
	return &controlapi.ListWorkersResponse{Record: []*apitypes.WorkerRecord{{Platforms: platforms}}}, nil
}

func (s *buildKitPlatformServer) ListenBuildHistory(request *controlapi.BuildHistoryRequest, stream grpc.ServerStreamingServer[controlapi.BuildHistoryEvent]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyCalls++
	if !request.ActiveOnly || !request.EarlyExit || request.Limit != 0 {
		return errors.New("refresh must inspect all active builds, not limited historical records")
	}
	if s.historyErr != nil {
		return s.historyErr
	}
	if s.active || s.completed {
		record := &controlapi.BuildHistoryRecord{Ref: "synthetic-build"}
		if s.completed {
			record.CompletedAt = timestamppb.Now()
		}
		return stream.Send(&controlapi.BuildHistoryEvent{Record: record})
	}
	return nil
}

func buildKitControlTestDialer(t *testing.T, service controlapi.ControlServer) func(context.Context) (net.Conn, error) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	controlapi.RegisterControlServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.DialContext
}

func TestRefreshLimaBuildKitPlatforms(t *testing.T) {
	for _, test := range []struct {
		name        string
		ready       bool
		active      bool
		completed   bool
		noWorkers   bool
		workerErr   error
		historyErr  error
		restartErr  error
		staysStale  bool
		wantRestart bool
		wantError   string
	}{
		{name: "already current", ready: true},
		{name: "idle stale worker", wantRestart: true},
		{name: "active build is preserved", active: true, wantError: "finish active builds"},
		{name: "completed build does not block", completed: true, wantRestart: true},
		{name: "worker discovery failure", workerErr: errors.New("worker probe failed"), wantError: "worker probe failed"},
		{name: "no workers", noWorkers: true, wantError: "no workers"},
		{name: "history failure", historyErr: errors.New("active-build probe failed"), wantError: "active-build probe failed"},
		{name: "restart failure", restartErr: errors.New("service restart failed"), wantRestart: true, wantError: "service restart failed"},
		{name: "post-refresh verification fails", staysStale: true, wantRestart: true, wantError: "still does not report"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &buildKitPlatformServer{
				ready: test.ready, active: test.active, completed: test.completed, noWorkers: test.noWorkers,
				workerErr: test.workerErr, historyErr: test.historyErr,
			}
			restarts := 0
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				if command.Name != "limactl" || strings.Join(command.Args, " ") != "shell --workdir=/ porto-engine -- systemctl --user restart default-buildkit.service" {
					return nil, errors.New("refresh attempted to restart something other than Porto BuildKit")
				}
				restarts++
				service.mu.Lock()
				service.ready = !test.staysStale
				service.mu.Unlock()
				return nil, test.restartErr
			}}
			manager := NewWithStateDir(runner, t.TempDir())
			manager.dialBuildKit = buildKitControlTestDialer(t, service)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := manager.refreshLimaBuildKitPlatforms(ctx)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("refresh = %v, want %q", err, test.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if test.active && !errors.Is(err, ErrConflict) {
				t.Fatalf("active build error = %v, want ErrConflict", err)
			}
			if restarts > 1 || (restarts == 1) != test.wantRestart {
				t.Fatalf("BuildKit restarts = %d, want restart = %t", restarts, test.wantRestart)
			}
			if test.ready {
				service.mu.Lock()
				calls := service.historyCalls
				service.mu.Unlock()
				if calls != 0 {
					t.Fatal("already current worker unnecessarily inspected active builds")
				}
			}
		})
	}
}
