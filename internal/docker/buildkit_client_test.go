package docker

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type delayedDialBuildKitControl struct {
	controlapi.UnimplementedControlServer
}

func (*delayedDialBuildKitControl) Info(context.Context, *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	return &controlapi.InfoResponse{}, nil
}

func TestBuildKitConnectionAllowsSlowTunnelSetupWithoutExtendingRPCDeadlines(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		deadline time.Duration
		want     codes.Code
	}{
		{"slow-SSH-startup", 45 * time.Second, codes.OK},
		{"shorter-caller-deadline", 10 * time.Second, codes.DeadlineExceeded},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				listener := bufconn.Listen(1 << 20)
				defer listener.Close()
				server := grpc.NewServer()
				controlapi.RegisterControlServer(server, &delayedDialBuildKitControl{})
				go func() { _ = server.Serve(listener) }()
				defer server.Stop()
				var attempts atomic.Int32
				connection, err := newBuildKitControlConnection(func(ctx context.Context) (net.Conn, error) {
					attempts.Add(1)
					timer := time.NewTimer(25 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return nil, context.Cause(ctx)
					case <-timer.C:
						return listener.DialContext(ctx)
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				ctx, cancel := context.WithTimeout(context.Background(), scenario.deadline)
				defer cancel()
				started := time.Now()
				_, err = controlapi.NewControlClient(connection).Info(ctx, &controlapi.InfoRequest{})
				if status.Code(err) != scenario.want {
					t.Fatalf("BuildKit Info = %v, want %s", err, scenario.want)
				}
				if attempts.Load() != 1 {
					t.Fatalf("slow tunnel setup was discarded and restarted %d times", attempts.Load())
				}
				wantElapsed := min(25*time.Second, scenario.deadline)
				if elapsed := time.Since(started); elapsed != wantElapsed {
					t.Fatalf("elapsed = %s, want %s", elapsed, wantElapsed)
				}
			})
		})
	}
}
