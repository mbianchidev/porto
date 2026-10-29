package docker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client/buildid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

type admissionBuildKitState struct {
	started chan struct{}
	ping    chan struct{}
	status  chan struct{}
	finish  chan struct{}
	once    sync.Once
}

type admissionBuildKitControl struct {
	controlapi.UnimplementedControlServer
	state *admissionBuildKitState
}

func (s *admissionBuildKitControl) Info(context.Context, *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	return &controlapi.InfoResponse{}, nil
}

func (s *admissionBuildKitControl) Solve(ctx context.Context, request *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	if request.Ref != "synthetic-build" {
		return nil, errors.New("unexpected synthetic build reference")
	}
	s.state.once.Do(func() { close(s.state.started) })
	select {
	case <-s.state.finish:
		return &controlapi.SolveResponse{}, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (s *admissionBuildKitControl) Status(request *controlapi.StatusRequest, stream grpc.ServerStreamingServer[controlapi.StatusResponse]) error {
	select {
	case s.state.status <- struct{}{}:
	default:
	}
	if request.Ref != "synthetic-build" {
		return errors.New("status lost its synthetic build reference")
	}
	select {
	case <-s.state.started:
		return stream.Send(&controlapi.StatusResponse{})
	case <-time.After(time.Second):
		return status.Error(codes.NotFound, "no such job synthetic-build")
	case <-stream.Context().Done():
		return context.Cause(stream.Context())
	}
}

type admissionBuildKitGateway struct {
	state *admissionBuildKitState
}

func (s *admissionBuildKitGateway) Ping(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	select {
	case s.state.ping <- struct{}{}:
	default:
	}
	if buildid.FromIncomingContext(ctx) != "synthetic-build" {
		return nil, errors.New("gateway lost its synthetic build reference")
	}
	select {
	case <-s.state.started:
		return &emptypb.Empty{}, nil
	case <-time.After(time.Second):
		return nil, status.Error(codes.NotFound, "forwarding Ping: no such job synthetic-build")
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func admissionBuildKitBackend(t *testing.T) (*admissionBuildKitState, func(context.Context) (net.Conn, error)) {
	t.Helper()
	state := &admissionBuildKitState{
		started: make(chan struct{}), ping: make(chan struct{}, 1),
		status: make(chan struct{}, 1), finish: make(chan struct{}),
	}
	upstreamListener := bufconn.Listen(1 << 20)
	upstream := grpc.NewServer()
	controlapi.RegisterControlServer(upstream, &admissionBuildKitControl{state: state})
	gateway := &admissionBuildKitGateway{state: state}
	upstream.RegisterService(&grpc.ServiceDesc{
		ServiceName: "moby.buildkit.v1.frontend.LLBBridge",
		HandlerType: (*buildKitEchoService)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Ping",
			Handler: func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				request := &emptypb.Empty{}
				if err := decode(request); err != nil {
					return nil, err
				}
				return gateway.Ping(ctx, request)
			},
		}},
	}, gateway)
	go func() { _ = upstream.Serve(upstreamListener) }()
	t.Cleanup(func() {
		upstream.Stop()
		_ = upstreamListener.Close()
	})
	return state, upstreamListener.DialContext
}

func TestBuildKitGatewayReportsSolvePreflightFailureWithoutCreatingJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix Docker upgrade transport")
	}
	state, dial := admissionBuildKitBackend(t)
	manager := NewWithStateDir(&fakeRunner{}, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.dialBuildKit = dial
	directory, err := os.MkdirTemp("/tmp", "porto-buildkit-admission-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "docker.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := NewAPIServer(socket, NewAPI(manager, socket))
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		stop, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = server.Close(stop)
	})
	connection, err := grpc.NewClient("passthrough:///admission",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialDockerBuildKitUpgrade(ctx, socket, "/grpc", nil)
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	control := controlapi.NewControlClient(connection)
	if _, err := control.Info(ctx, &controlapi.InfoRequest{}); err != nil {
		t.Fatal(err)
	}
	_, release, err := manager.BeginDataTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseWrite := func() { releaseOnce.Do(release) }
	defer releaseWrite()
	go func() { _, _ = control.Solve(ctx, &controlapi.SolveRequest{Ref: "synthetic-build"}) }()
	pingDone := make(chan error, 1)
	go func() {
		err := connection.Invoke(buildid.AppendToOutgoingContext(ctx, "synthetic-build"),
			"/moby.buildkit.v1.frontend.LLBBridge/Ping", &emptypb.Empty{}, &emptypb.Empty{})
		pingDone <- err
	}()
	select {
	case <-state.ping:
		t.Fatal("gateway reached BuildKit before the corresponding Solve preflight could register its job")
	case <-time.After(50 * time.Millisecond):
	}
	releaseWrite()
	select {
	case err := <-pingDone:
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("gateway lost the Solve preflight refusal: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("gateway did not resume after its Solve preflight")
	}
	select {
	case <-state.started:
		t.Fatal("refused preflight created an upstream build job")
	default:
	}
}

func TestBuildKitGatewayWaitsForSolvePreflightWithoutLosingBuildIdentity(t *testing.T) {
	state, dial := admissionBuildKitBackend(t)
	upstream, err := newBuildKitControlConnection(dial)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	admissions := newBuildKitSolveAdmissions()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	proxy := &buildKitControlProxy{
		client: controlapi.NewControlClient(upstream), admissions: admissions,
		validateArchitecture: func(ctx context.Context, _ *controlapi.SolveRequest) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		},
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnknownServiceHandler(transparentBuildKitHandler(upstream, admissions)))
	controlapi.RegisterControlServer(server, proxy)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := newBuildKitControlConnection(listener.DialContext)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	solveDone := make(chan error, 1)
	go func() {
		_, err := controlapi.NewControlClient(connection).Solve(ctx, &controlapi.SolveRequest{Ref: "synthetic-build"})
		solveDone <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("Solve did not enter its preflight")
	}
	gatewayConnection, err := newBuildKitControlConnection(listener.DialContext)
	if err != nil {
		t.Fatal(err)
	}
	defer gatewayConnection.Close()
	pingDone := make(chan error, 1)
	go func() {
		pingDone <- gatewayConnection.Invoke(buildid.AppendToOutgoingContext(ctx, "synthetic-build"),
			"/moby.buildkit.v1.frontend.LLBBridge/Ping", &emptypb.Empty{}, &emptypb.Empty{})
	}()
	statusDone := make(chan error, 1)
	go func() {
		stream, err := controlapi.NewControlClient(connection).Status(ctx, &controlapi.StatusRequest{Ref: "synthetic-build"})
		if err == nil {
			_, err = stream.Recv()
		}
		statusDone <- err
	}()
	select {
	case <-state.ping:
		t.Fatal("gateway reached BuildKit before Solve preflight finished")
	case <-state.status:
		t.Fatal("status reached BuildKit before Solve preflight finished")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-pingDone:
		if err != nil {
			t.Fatalf("admitted gateway request failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("gateway did not resume after successful preflight")
	}
	select {
	case err := <-statusDone:
		if err != nil {
			t.Fatalf("admitted status request failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("status did not resume after successful preflight")
	}
	close(state.finish)
	select {
	case err := <-solveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Solve did not finish")
	}
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	if len(admissions.active) != 0 {
		t.Fatal("completed build admission was retained")
	}
}

func TestBuildKitAdmissionPreservesIndependentJobsCancellationAndRefusals(t *testing.T) {
	admissions := newBuildKitSolveAdmissions()
	first, err := admissions.begin("first")
	if err != nil {
		t.Fatal(err)
	}
	defer admissions.finish("first", first)
	if _, err := admissions.begin("first"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate reference replaced an active build admission: %v", err)
	}
	second, err := admissions.begin("second")
	if err != nil {
		t.Fatal(err)
	}
	defer admissions.finish("second", second)
	second.resolve(nil)
	if err := admissions.wait(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := admissions.wait(ctx, "first"); status.Code(err) != codes.Canceled {
		t.Fatalf("gateway ignored caller cancellation: %v", err)
	}
	first.resolve(status.Error(codes.FailedPrecondition, "synthetic architecture refusal"))
	if err := admissions.wait(context.Background(), "first"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("gateway replaced the actual preflight failure: %v", err)
	}
}
