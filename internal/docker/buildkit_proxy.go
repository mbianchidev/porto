package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type buildKitControlProxy struct {
	controlapi.UnimplementedControlServer
	client              controlapi.ControlClient
	containerdNamespace string
	imageExported       func()

	workerMu         sync.Mutex
	workerCompatible bool
}

func (p *buildKitControlProxy) DiskUsage(ctx context.Context, request *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	var header, trailer metadata.MD
	response, err := p.client.DiskUsage(
		outgoingBuildKitContext(ctx),
		request,
		grpc.Header(&header),
		grpc.Trailer(&trailer),
	)
	forwardBuildKitUnaryMetadata(ctx, header, trailer)
	return response, err
}

func (p *buildKitControlProxy) Prune(request *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	upstream, err := p.client.Prune(outgoingBuildKitContext(stream.Context()), request)
	if err != nil {
		return err
	}
	return forwardBuildKitServerStream(upstream, stream)
}

func (p *buildKitControlProxy) Solve(ctx context.Context, request *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	rewritten, hasMoby, err := rewriteMobyExporters(request)
	if err != nil {
		return nil, err
	}
	if hasMoby {
		if err := p.ensureMobyWorker(ctx); err != nil {
			return nil, err
		}
	}
	var header, trailer metadata.MD
	response, err := p.client.Solve(
		outgoingBuildKitContext(ctx),
		rewritten,
		grpc.Header(&header),
		grpc.Trailer(&trailer),
	)
	forwardBuildKitUnaryMetadata(ctx, header, trailer)
	if err == nil && hasMoby && p.imageExported != nil {
		p.imageExported()
	}
	return response, err
}

func (p *buildKitControlProxy) ensureMobyWorker(ctx context.Context) error {
	p.workerMu.Lock()
	if p.workerCompatible {
		p.workerMu.Unlock()
		return nil
	}
	p.workerMu.Unlock()

	response, err := p.client.ListWorkers(outgoingBuildKitContext(ctx), &controlapi.ListWorkersRequest{})
	if err != nil {
		return fmt.Errorf("inspect BuildKit workers for moby export: %w", err)
	}
	for _, worker := range response.GetRecord() {
		if worker.GetLabels()[buildKitWorkerExecutorLabel] != buildKitWorkerContainerdExecutor {
			continue
		}
		if worker.GetLabels()[buildKitWorkerContainerdNamespaceLabel] != p.containerdNamespace {
			continue
		}
		p.workerMu.Lock()
		p.workerCompatible = true
		p.workerMu.Unlock()
		return nil
	}
	return fmt.Errorf(
		"%w: BuildKit moby exporter requires a containerd worker in Porto namespace %q",
		ErrUnsupported,
		p.containerdNamespace,
	)
}

func (p *buildKitControlProxy) Status(request *controlapi.StatusRequest, stream grpc.ServerStreamingServer[controlapi.StatusResponse]) error {
	upstream, err := p.client.Status(outgoingBuildKitContext(stream.Context()), request)
	if err != nil {
		return err
	}
	return forwardBuildKitServerStream(upstream, stream)
}

func (p *buildKitControlProxy) Session(stream grpc.BidiStreamingServer[controlapi.BytesMessage, controlapi.BytesMessage]) error {
	upstream, err := p.client.Session(outgoingBuildKitContext(stream.Context()))
	if err != nil {
		return err
	}
	sendDone := make(chan error, 1)
	receiveDone := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					err = upstream.CloseSend()
				}
				sendDone <- err
				return
			}
			if err := upstream.Send(message); err != nil {
				sendDone <- err
				return
			}
		}
	}()
	go func() {
		if header, err := upstream.Header(); err != nil {
			receiveDone <- err
			return
		} else if len(header) > 0 {
			if err := stream.SetHeader(header); err != nil {
				receiveDone <- err
				return
			}
		}
		for {
			message, err := upstream.Recv()
			if err != nil {
				stream.SetTrailer(upstream.Trailer())
				if err == io.EOF {
					err = nil
				}
				receiveDone <- err
				return
			}
			if err := stream.Send(message); err != nil {
				receiveDone <- err
				return
			}
		}
	}()
	for {
		select {
		case err := <-sendDone:
			if err != nil {
				return err
			}
			sendDone = nil
		case err := <-receiveDone:
			return err
		}
	}
}

func (p *buildKitControlProxy) ListWorkers(ctx context.Context, request *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	var header, trailer metadata.MD
	response, err := p.client.ListWorkers(
		outgoingBuildKitContext(ctx),
		request,
		grpc.Header(&header),
		grpc.Trailer(&trailer),
	)
	forwardBuildKitUnaryMetadata(ctx, header, trailer)
	return response, err
}

func (p *buildKitControlProxy) Info(ctx context.Context, request *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	var header, trailer metadata.MD
	response, err := p.client.Info(
		outgoingBuildKitContext(ctx),
		request,
		grpc.Header(&header),
		grpc.Trailer(&trailer),
	)
	forwardBuildKitUnaryMetadata(ctx, header, trailer)
	return response, err
}

func (p *buildKitControlProxy) ListenBuildHistory(request *controlapi.BuildHistoryRequest, stream grpc.ServerStreamingServer[controlapi.BuildHistoryEvent]) error {
	upstream, err := p.client.ListenBuildHistory(outgoingBuildKitContext(stream.Context()), request)
	if err != nil {
		return err
	}
	return forwardBuildKitServerStream(upstream, stream)
}

func (p *buildKitControlProxy) UpdateBuildHistory(ctx context.Context, request *controlapi.UpdateBuildHistoryRequest) (*controlapi.UpdateBuildHistoryResponse, error) {
	var header, trailer metadata.MD
	response, err := p.client.UpdateBuildHistory(
		outgoingBuildKitContext(ctx),
		request,
		grpc.Header(&header),
		grpc.Trailer(&trailer),
	)
	forwardBuildKitUnaryMetadata(ctx, header, trailer)
	return response, err
}

func outgoingBuildKitContext(ctx context.Context) context.Context {
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, incoming.Copy())
}

func forwardBuildKitUnaryMetadata(ctx context.Context, header, trailer metadata.MD) {
	if len(header) > 0 {
		_ = grpc.SetHeader(ctx, header)
	}
	if len(trailer) > 0 {
		_ = grpc.SetTrailer(ctx, trailer)
	}
}

func forwardBuildKitServerStream[T any](
	upstream grpc.ServerStreamingClient[T],
	stream grpc.ServerStreamingServer[T],
) error {
	header, err := upstream.Header()
	if err != nil {
		return err
	}
	if len(header) > 0 {
		if err := stream.SetHeader(header); err != nil {
			return err
		}
	}
	defer stream.SetTrailer(upstream.Trailer())
	for {
		message, err := upstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(message); err != nil {
			return err
		}
	}
}

var buildKitProxyStream = &grpc.StreamDesc{
	ServerStreams: true,
	ClientStreams: true,
}

func transparentBuildKitHandler(connection *grpc.ClientConn) grpc.StreamHandler {
	return func(_ any, serverStream grpc.ServerStream) error {
		method, ok := grpc.MethodFromServerStream(serverStream)
		if !ok {
			return status.Error(codes.Internal, "BuildKit proxy cannot determine the gRPC method")
		}
		upstreamContext, cancel := context.WithCancel(outgoingBuildKitContext(serverStream.Context()))
		defer cancel()
		clientStream, err := connection.NewStream(upstreamContext, buildKitProxyStream, method)
		if err != nil {
			return err
		}

		serverToClient := forwardBuildKitUnknownServerToClient(serverStream, clientStream)
		clientToServer := forwardBuildKitUnknownClientToServer(clientStream, serverStream)
		for range 2 {
			select {
			case err := <-serverToClient:
				if err == io.EOF {
					if err := clientStream.CloseSend(); err != nil {
						return err
					}
					serverToClient = nil
					continue
				}
				cancel()
				return status.Errorf(codes.Internal, "forward BuildKit request: %v", err)
			case err := <-clientToServer:
				serverStream.SetTrailer(clientStream.Trailer())
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
		return status.Error(codes.Internal, "BuildKit proxy stream ended without a response")
	}
}

func forwardBuildKitUnknownServerToClient(source grpc.ServerStream, destination grpc.ClientStream) <-chan error {
	result := make(chan error, 1)
	go func() {
		for {
			frame := &emptypb.Empty{}
			if err := source.RecvMsg(frame); err != nil {
				result <- err
				return
			}
			if err := destination.SendMsg(frame); err != nil {
				result <- err
				return
			}
		}
	}()
	return result
}

func forwardBuildKitUnknownClientToServer(source grpc.ClientStream, destination grpc.ServerStream) <-chan error {
	result := make(chan error, 1)
	go func() {
		first := true
		for {
			frame := &emptypb.Empty{}
			if err := source.RecvMsg(frame); err != nil {
				result <- err
				return
			}
			if first {
				header, err := source.Header()
				if err != nil {
					result <- err
					return
				}
				if len(header) > 0 {
					if err := destination.SendHeader(header); err != nil {
						result <- err
						return
					}
				}
				first = false
			}
			if err := destination.SendMsg(frame); err != nil {
				result <- err
				return
			}
		}
	}()
	return result
}

var _ controlapi.ControlServer = (*buildKitControlProxy)(nil)

type singleConnListener struct {
	connection net.Conn
	accepted   chan struct{}
	closed     chan struct{}
	acceptOnce sync.Once
	closeOnce  sync.Once
}

func newSingleConnListener(connection net.Conn) *singleConnListener {
	return &singleConnListener{
		connection: connection,
		accepted:   make(chan struct{}),
		closed:     make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	accepted := false
	l.acceptOnce.Do(func() {
		accepted = true
		close(l.accepted)
	})
	if accepted {
		return &listenerConn{Conn: l.connection, closeListener: l.Close}, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		close(l.closed)
		closeErr = l.connection.Close()
	})
	if errors.Is(closeErr, net.ErrClosed) {
		return nil
	}
	return closeErr
}

func (l *singleConnListener) Addr() net.Addr {
	return l.connection.LocalAddr()
}

type listenerConn struct {
	net.Conn
	closeOnce     sync.Once
	closeErr      error
	closeListener func() error
}

func (c *listenerConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.closeListener()
	})
	return c.closeErr
}

var _ net.Listener = (*singleConnListener)(nil)
