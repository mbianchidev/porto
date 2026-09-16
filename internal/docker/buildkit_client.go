package docker

import (
	"context"
	"net"

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
