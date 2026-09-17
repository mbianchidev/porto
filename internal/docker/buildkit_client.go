package docker

import (
	"context"
	"fmt"
	"io"
	"net"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func newBuildKitControlConnection(dial func(context.Context) (net.Conn, error)) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"passthrough:///buildkit",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dial(ctx)
		}),
	)
}

func ensureBuildKitIdle(ctx context.Context, client controlapi.ControlClient) error {
	stream, err := client.ListenBuildHistory(ctx, &controlapi.BuildHistoryRequest{
		ActiveOnly: true,
		EarlyExit:  true,
	}, grpc.WaitForReady(true))
	if err != nil {
		return fmt.Errorf("inspect active BuildKit builds: %w", err)
	}
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read active BuildKit builds: %w", err)
		}
		if event.Type != controlapi.BuildHistoryEventType_DELETED && event.Record != nil && event.Record.CompletedAt == nil {
			return fmt.Errorf("%w: BuildKit has an active build; finish active builds and retry", ErrConflict)
		}
	}
}
